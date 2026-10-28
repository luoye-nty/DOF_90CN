package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfmemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/statbuff"
	"longheng.io/server/internal/platform/timequeue"
)

// 城镇增益药剂的"到期摘图标"定时器（stat_change_potion_expiry.go）。
//
// 服主 2026-10-03 的问题："时限过了以后会不会正确的结束效果？结束后会不会清图标？"
//
// 三条都要钉住：
//  1. 武装时按**角色记录里的真实到期时刻**排延迟（不是猜的时长）；
//  2. 到点且窗口确实过期 → CHARACTER_DEL_BUFF(这一族全部 id) + BUFF_DUNGEON(空列表)；
//  3. 到点但窗口已被另一瓶续上 → **不摘图标**，按新的到期时间重新武装；
//     以及作废过的旧回调（换档/取消）不许再动图标。

// ---- 测试替身 ----

type statChangePotionTestSource map[string]string

func (s statChangePotionTestSource) ReadText(relativePath string) (string, error) {
	text, ok := s[relativePath]
	if !ok {
		return "", errors.New("missing member " + relativePath)
	}
	return text, nil
}

// statChangePotionTestQueue 是 gameplayTimeQueue 的手动版：记录排期，由测试自己触发。
type statChangePotionTestQueue struct {
	mu        sync.Mutex
	now       time.Time
	delays    map[string]time.Duration
	callbacks map[string]timequeue.Callback
	order     []string
	cancelled []string
}

func newStatChangePotionTestQueue(now time.Time) *statChangePotionTestQueue {
	return &statChangePotionTestQueue{
		now:       now,
		delays:    make(map[string]time.Duration),
		callbacks: make(map[string]timequeue.Callback),
	}
}

func (q *statChangePotionTestQueue) Now() time.Time { return q.now }

func (q *statChangePotionTestQueue) ScheduleAfter(name string, delay time.Duration, cb timequeue.Callback) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.delays[name] = delay
	q.callbacks[name] = cb
	q.order = append(q.order, name)
	return nil
}

func (q *statChangePotionTestQueue) RegisterDaily(string, int, int, timequeue.Callback) error {
	return nil
}

func (q *statChangePotionTestQueue) Cancel(name string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cancelled = append(q.cancelled, name)
	delete(q.callbacks, name)
	delete(q.delays, name)
	return true
}

func (q *statChangePotionTestQueue) Start(context.Context) {}

func (q *statChangePotionTestQueue) fire(t *testing.T, name string) {
	t.Helper()
	q.mu.Lock()
	cb, ok := q.callbacks[name]
	q.mu.Unlock()
	if !ok {
		t.Fatalf("timer %q was not scheduled (order=%v cancelled=%v)", name, q.order, q.cancelled)
	}
	cb(q.now)
}

func (q *statChangePotionTestQueue) lastScheduled(t *testing.T) string {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.order) == 0 {
		t.Fatal("nothing was scheduled")
	}
	return q.order[len(q.order)-1]
}

// 只放 buff 1136 的定义表；道具表给一件药剂即可（到期清除用的是 ByBuff，不是 ByItem）。
const statChangePotionTestBuffList = "[index]\n1136 \n[system group]\n`random_box_buff` \n[icon index]\n183 \n" +
	"[tooltip text]\n`史诗道具掉落率提升` \n[effect]\n[name]\n`epic_rarity_item_drop_rate` \n" +
	"[param]\n100 \n[/param]\n[/effect]\n[apply type]\n[type]\n`expire_time` \n[/apply type]\n[/index]\n"

const statChangePotionTestStackableList = "[index]\n490002458 \n[probability buff list]\n1136 100 \n[/index]\n"

func newStatChangePotionTestService(
	t *testing.T,
	repositories dnfrepo.Group,
	queue *statChangePotionTestQueue,
) (*Service, *gameSession) {
	t.Helper()
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameplayTimers:     queue,
		pvfItemCatalog: &pvfDungeonDropCatalog{source: statChangePotionTestSource{
			currentVariousBuffList:          statChangePotionTestBuffList,
			currentVariousBuffStackableList: statChangePotionTestStackableList,
		}},
	}
	session := &gameSession{
		conn:                connection,
		connID:              "stat-change-potion-expiry-test",
		selectedCharacterID: 11,
		characterGeneration: 7,
	}
	return service, session
}

func saveStatChangePotionCharacter(t *testing.T, repositories dnfrepo.Group, expireAt time.Time) {
	t.Helper()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "11",
		AccountID:   "dnf:1",
		Stats: map[string]int64{
			statbuff.EpicDropRatePercentStatKey: 200,
			statbuff.EpicDropRateExpireStatKey:  expireAt.Unix(),
			statbuff.EpicDropRateItemStatKey:    490002458,
			statbuff.EpicDropRateBuffIDStatKey:  1136,
		},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestStatChangePotionExpiryTimerArmsOnTheRealExpiry：延迟 = 到期时刻 - 现在。
func TestStatChangePotionExpiryTimerArmsOnTheRealExpiry(t *testing.T) {
	now := time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC)
	queue := newStatChangePotionTestQueue(now)
	repositories := dnfmemory.NewMemoryGroup()
	saveStatChangePotionCharacter(t, repositories, now.Add(10*time.Minute))
	service, session := newStatChangePotionTestService(t, repositories, queue)

	service.armCurrentStatChangePotionTimer(session, 11, now.Add(10*time.Minute), "test")
	name := queue.lastScheduled(t)
	if got := queue.delays[name]; got != 10*time.Minute {
		t.Fatalf("delay=%s want 10m", got)
	}
	if !session.statChangePotionTimerActive || session.statChangePotionTimerCharacterID != 11 {
		t.Fatalf("timer state active=%t char=%d", session.statChangePotionTimerActive,
			session.statChangePotionTimerCharacterID)
	}
}

// TestStatChangePotionExpiryTimerClearsTheBuffIcon：到期确实过期 → 摘图标。
func TestStatChangePotionExpiryTimerClearsTheBuffIcon(t *testing.T) {
	now := time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC)
	queue := newStatChangePotionTestQueue(now)
	repositories := dnfmemory.NewMemoryGroup()
	// 窗口已经在 now 之前结束（模拟"到点时确实过期"）。
	saveStatChangePotionCharacter(t, repositories, now.Add(-time.Second))
	service, session := newStatChangePotionTestService(t, repositories, queue)

	service.armCurrentStatChangePotionTimer(session, 11, now.Add(-time.Second), "test")
	name := queue.lastScheduled(t)
	queue.fire(t, name)

	// 第一发：CHARACTER_DEL_BUFF(这一族全部 id) —— 本测试的 PVF 源里只有 1136。
	del, rest := splitLongHengGameServerUpperPacket(t, session.conn.(*bufferConn).write.Bytes())
	if del.Header.Classification != 0 || del.Header.MsgID != 488 {
		t.Fatalf("del header=%+v want class0/op488", del.Header)
	}
	if len(del.Body) != 5 || del.Body[0] != 1 || binary.LittleEndian.Uint32(del.Body[1:5]) != 1136 {
		t.Fatalf("del body=%x want u8 count 1 + u32 1136", del.Body)
	}
	// 第二发：CHARACTER_BUFF_DUNGEON(空列表) —— 清掉本地激活标志。
	activate, rest := splitLongHengGameServerUpperPacket(t, rest)
	if len(rest) != 0 {
		t.Fatalf("unexpected trailing bytes: %x", rest)
	}
	if activate.Header.Classification != 0 || activate.Header.MsgID != 489 {
		t.Fatalf("activate header=%+v want class0/op489", activate.Header)
	}
	if len(activate.Body) != 2 || binary.LittleEndian.Uint16(activate.Body) != 0 {
		t.Fatalf("activate body=%x want u16 count 0", activate.Body)
	}
	if session.statChangePotionTimerActive {
		t.Fatal("timer must be disarmed after firing")
	}
}

// TestStatChangePotionExpiryTimerRearmsWhenTheWindowWasRefreshed：
// 到点时窗口被另一瓶续上了 —— 不许摘图标，按新到期时间重新武装。
func TestStatChangePotionExpiryTimerRearmsWhenTheWindowWasRefreshed(t *testing.T) {
	now := time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC)
	queue := newStatChangePotionTestQueue(now)
	repositories := dnfmemory.NewMemoryGroup()
	saveStatChangePotionCharacter(t, repositories, now.Add(5*time.Minute))
	service, session := newStatChangePotionTestService(t, repositories, queue)

	service.armCurrentStatChangePotionTimer(session, 11, now.Add(time.Minute), "first")
	first := queue.lastScheduled(t)
	queue.fire(t, first)

	if session.conn.(*bufferConn).write.Len() != 0 {
		t.Fatalf("a refreshed window must not clear the icon: %x",
			session.conn.(*bufferConn).write.Bytes())
	}
	second := queue.lastScheduled(t)
	if second == first {
		t.Fatal("the timer was not re-armed")
	}
	if got := queue.delays[second]; got != 5*time.Minute {
		t.Fatalf("re-armed delay=%s want 5m (the refreshed expiry)", got)
	}
	if !session.statChangePotionTimerActive {
		t.Fatal("timer must stay armed for the refreshed window")
	}
}

// TestStatChangePotionExpiryTimerIgnoresCancelledGeneration：
// 换档/取消之后，旧回调再跑也不许动图标。
func TestStatChangePotionExpiryTimerIgnoresCancelledGeneration(t *testing.T) {
	now := time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC)
	queue := newStatChangePotionTestQueue(now)
	repositories := dnfmemory.NewMemoryGroup()
	saveStatChangePotionCharacter(t, repositories, now.Add(-time.Second))
	service, session := newStatChangePotionTestService(t, repositories, queue)

	service.armCurrentStatChangePotionTimer(session, 11, now.Add(time.Minute), "first")
	stale := queue.lastScheduled(t)
	service.cancelCurrentStatChangePotionTimer(session, "tier_replaced")

	// 旧回调此刻才跑（真实队列里它已经被 Cancel，这里直接手动触发以验证守卫）。
	queue.mu.Lock()
	cb := queue.callbacks[stale]
	queue.mu.Unlock()
	if cb != nil {
		cb(now)
	}
	if session.conn.(*bufferConn).write.Len() != 0 {
		t.Fatalf("a cancelled timer must not touch the icon: %x",
			session.conn.(*bufferConn).write.Bytes())
	}
	if session.statChangePotionTimerActive {
		t.Fatal("cancelled timer must stay disarmed")
	}
}
