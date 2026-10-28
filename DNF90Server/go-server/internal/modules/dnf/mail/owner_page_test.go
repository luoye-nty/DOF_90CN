package mail

import (
	"context"
	"strconv"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 本文件钉住 Open() 的**分页窗口取哪一头**。
//
// 背景（实机 2026-09-23，角色 5「随便玩玩」）：451 封活着的邮件、窗口恒为
// mail_id 516..535，GM 新发的 966 落库成功却永远进不了窗口，玩家侧的表现就是
// "收不到邮件"。原因见 owner.go Open() 里的注释：窗口只发一页、协议没有翻页，
// 而老实现 normal[:20] 取的是**最老**的 20 封，于是窗口被最老那批永久钉死。
//
// 这些用例不测"顺序"（发出去的仍是升序，客户端自己重排），只测**选中的集合**。

// pagedMailboxFixture 造一个只依赖邮箱仓库的角色：normalCount 封普通邮件后接
// savedCount 封"保存的"邮件，id 从 1 递增，created_at 每封 +1 秒 —— 所以
// **id 越大越新**，窗口该取哪一头一眼可辨。
type pagedMailboxFixture struct {
	repos       dnfrepo.Group
	characterID string
}

func newPagedMailboxFixture(t *testing.T, characterID string, normalCount, savedCount int) pagedMailboxFixture {
	t.Helper()
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	// 放在 1 小时前，确保既不过期、也不落在未来。
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	mails := make(map[string]dnfrepo.MailRecord, normalCount+savedCount)
	for i := 1; i <= normalCount+savedCount; i++ {
		id := strconv.Itoa(i)
		record := dnfrepo.MailRecord{
			MailID:      id,
			SenderName:  "系统",
			Title:       "分页测试邮件",
			Gold:        int64(i),
			CreatedAt:   base.Add(time.Duration(i) * time.Second),
			Attachments: []dnfrepo.MailAttachment{{ItemID: 1001, Count: 1}},
		}
		if i > normalCount {
			record.Metadata = map[string]string{"mailbox_saved": "true"}
		}
		mails[id] = record
	}
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: characterID,
		Mails:       mails,
		UpdatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}
	return pagedMailboxFixture{
		repos:       repos,
		characterID: characterID,
	}
}

// appendNewest 追加一封严格最新的普通邮件，返回它的 mail_id。
func (f pagedMailboxFixture) appendNewest(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	record, found, err := f.repos.Mailbox.Load(ctx, f.characterID)
	if err != nil || !found {
		t.Fatalf("load mailbox found=%t err=%v", found, err)
	}
	newest := 0
	for id := range record.Mails {
		if n, convErr := strconv.Atoi(id); convErr == nil && n > newest {
			newest = n
		}
	}
	id := strconv.Itoa(newest + 1)
	record.Mails[id] = dnfrepo.MailRecord{
		MailID:      id,
		SenderName:  "系统",
		Title:       "分页测试邮件",
		CreatedAt:   time.Now().UTC(),
		Attachments: []dnfrepo.MailAttachment{{ItemID: 1001, Count: 1}},
	}
	record.UpdatedAt = time.Now().UTC()
	if err := f.repos.Mailbox.Save(ctx, record); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}
	return id
}

func (f pagedMailboxFixture) open(t *testing.T) OpenResult {
	t.Helper()
	result, err := NewOwner(f.repos).Open(context.Background(), f.characterID)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return result
}

func mailboxMailIDs(mails []dnfrepo.MailRecord) []string {
	out := make([]string, 0, len(mails))
	for _, mail := range mails {
		out = append(out, mail.MailID)
	}
	return out
}

func idRange(from, to int) []string {
	out := make([]string, 0, to-from+1)
	for i := from; i <= to; i++ {
		out = append(out, strconv.Itoa(i))
	}
	return out
}

func assertMailIDs(t *testing.T, label string, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v (len %d), want %v (len %d)", label, got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v (first mismatch at index %d)", label, got, want, i)
		}
	}
}

// 超过一页时，留下的必须是**最新**的 20 封；窗口内仍保持升序，字节布局不变。
func TestMailboxOpenPageKeepsNewestNormalMails(t *testing.T) {
	fixture := newPagedMailboxFixture(t, "77", 25, 0)

	opened := fixture.open(t)

	assertMailIDs(t, "page", mailboxMailIDs(opened.Mails), idRange(6, 25))
	if opened.NotLoaded != 5 {
		t.Fatalf("NotLoaded = %d, want 5", opened.NotLoaded)
	}
	// 全量统计不能跟着分页一起缩水。
	if opened.Total != 25 || opened.Unread != 25 || opened.Claimable != 25 {
		t.Fatalf("total=%d unread=%d claimable=%d, want 25/25/25", opened.Total, opened.Unread, opened.Claimable)
	}
}

// 保存的邮件同理：留最新的 10 封。
func TestMailboxOpenPageKeepsNewestSavedMails(t *testing.T) {
	fixture := newPagedMailboxFixture(t, "78", 0, 12)

	opened := fixture.open(t)

	assertMailIDs(t, "page", mailboxMailIDs(opened.Mails), idRange(3, 12))
	if opened.NotLoaded != 2 {
		t.Fatalf("NotLoaded = %d, want 2", opened.NotLoaded)
	}
	if opened.Total != 12 {
		t.Fatalf("Total = %d, want 12", opened.Total)
	}
}

// 普通页与保存页各有各的窗口，合起来顺序不变：先是普通页，再是保存页。
func TestMailboxOpenPageKeepsNormalAndSavedWindowsSeparate(t *testing.T) {
	fixture := newPagedMailboxFixture(t, "79", 25, 12)

	opened := fixture.open(t)

	want := append(idRange(6, 25), idRange(28, 37)...)
	assertMailIDs(t, "page", mailboxMailIDs(opened.Mails), want)
	if opened.NotLoaded != 7 {
		t.Fatalf("NotLoaded = %d, want 7 (5 normal + 2 saved)", opened.NotLoaded)
	}
	if opened.Total != 37 {
		t.Fatalf("Total = %d, want 37", opened.Total)
	}
}

// 回归：这是实机那次事故的形状。积压远超一页时，**新来的那封信必须看得见**。
// 老实现下窗口恒为最老的 20 封，新信永远进不来 —— 玩家看到的就是"收不到邮件"。
func TestMailboxOpenKeepsNewLetterVisibleBehindALargeBacklog(t *testing.T) {
	const backlog = 450
	fixture := newPagedMailboxFixture(t, "80", backlog, 0)

	before := fixture.open(t)
	assertMailIDs(t, "backlog page", mailboxMailIDs(before.Mails), idRange(backlog-19, backlog))
	if before.NotLoaded != backlog-20 {
		t.Fatalf("NotLoaded = %d, want %d", before.NotLoaded, backlog-20)
	}

	freshID := fixture.appendNewest(t)

	after := fixture.open(t)
	found := false
	for _, id := range mailboxMailIDs(after.Mails) {
		if id == freshID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("新邮件 %s 不在窗口里：page=%v", freshID, mailboxMailIDs(after.Mails))
	}
	// 窗口滑动：最老的那封被挤出去，长度不变。
	if len(after.Mails) != mailboxNormalPageSize {
		t.Fatalf("page len = %d, want %d", len(after.Mails), mailboxNormalPageSize)
	}
	assertMailIDs(t, "slid page", mailboxMailIDs(after.Mails), idRange(backlog-18, backlog+1))
	if after.NotLoaded != backlog-19 {
		t.Fatalf("NotLoaded = %d, want %d", after.NotLoaded, backlog-19)
	}
}

// 不到一页时逐封全给，且删除/过期的仍然不参与窗口，也不算进 NotLoaded。
func TestMailboxOpenPageUnderOnePageReturnsEveryLiveMail(t *testing.T) {
	fixture := newPagedMailboxFixture(t, "81", 3, 0)

	ctx := context.Background()
	record, found, err := fixture.repos.Mailbox.Load(ctx, fixture.characterID)
	if err != nil || !found {
		t.Fatalf("load mailbox found=%t err=%v", found, err)
	}
	deleted := record.Mails["1"]
	deleted.Deleted = true
	record.Mails["1"] = deleted
	expired := record.Mails["2"]
	expired.CreatedAt = time.Now().UTC().Add(-(mailboxNormalLifetime + time.Hour))
	record.Mails["2"] = expired
	if err := fixture.repos.Mailbox.Save(ctx, record); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}

	opened := fixture.open(t)

	assertMailIDs(t, "page", mailboxMailIDs(opened.Mails), []string{"3"})
	if opened.NotLoaded != 0 {
		t.Fatalf("NotLoaded = %d, want 0", opened.NotLoaded)
	}
	if opened.Total != 1 {
		t.Fatalf("Total = %d, want 1", opened.Total)
	}
}
