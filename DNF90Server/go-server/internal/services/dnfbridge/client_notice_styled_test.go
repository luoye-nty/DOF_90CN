package dnfbridge

import (
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

// 版本 2 的头部布局必须和 90CN.cpp 的读法逐字节对上：type, version, u16 单元数,
// u32 ARGB, u8 样式, u8 保留, 然后正文。
func TestBuildCurrentClientNoticeStyledBodyMatchesTheDLLReader(t *testing.T) {
	const text = "原生窗口\n第二行"
	body, err := buildCurrentClientNoticeStyledBody(
		text, currentClientNoticeDefaultColor, currentClientNoticeStyleNativeWindow)
	if err != nil {
		t.Fatal(err)
	}
	if body[0] != currentClientNoticeType {
		t.Fatalf("styled notice type=%d want %d", body[0], currentClientNoticeType)
	}
	if body[1] != currentClientNoticeVersionStyled {
		t.Fatalf("styled notice version=%d want %d", body[1], currentClientNoticeVersionStyled)
	}
	units := utf16.Encode([]rune(text))
	if got := binary.LittleEndian.Uint16(body[2:4]); got != uint16(len(units)) {
		t.Fatalf("styled notice unit count=%d want %d", got, len(units))
	}
	if got := binary.LittleEndian.Uint32(body[4:8]); got != currentClientNoticeDefaultColor {
		t.Fatalf("styled notice color=%08x", got)
	}
	if body[8] != currentClientNoticeStyleNativeWindow || body[9] != 0 {
		t.Fatalf("styled notice style bytes=%x", body[8:10])
	}
	if len(body) != currentClientNoticeStyledHeaderBytes+len(units)*2 {
		t.Fatalf("styled notice body length=%d", len(body))
	}
	for index, unit := range units {
		offset := currentClientNoticeStyledHeaderBytes + index*2
		if binary.LittleEndian.Uint16(body[offset:offset+2]) != unit {
			t.Fatalf("styled notice text mismatch at unit %d", index)
		}
	}
}

// 版本 1 必须一个字节都不变：客户端仍然接受它并按覆盖层处理，回滚任意一侧只会
// 让样子退化，不会丢消息。
func TestBuildCurrentClientNoticeBodyStaysVersionOne(t *testing.T) {
	body, err := buildCurrentClientNoticeBody("覆盖层", 0xFF112233)
	if err != nil {
		t.Fatal(err)
	}
	if body[1] != currentClientNoticeVersion {
		t.Fatalf("overlay notice version=%d want %d", body[1], currentClientNoticeVersion)
	}
	units := utf16.Encode([]rune("覆盖层"))
	if len(body) != currentClientNoticeHeaderBytes+len(units)*2 {
		t.Fatalf("overlay notice body length=%d", len(body))
	}
}

func TestBuildCurrentClientNoticeStyledBodyRejectsUnknownStyle(t *testing.T) {
	if _, err := buildCurrentClientNoticeStyledBody(
		"x", currentClientNoticeDefaultColor, 3,
	); err == nil {
		t.Fatal("an unknown client renderer style was accepted")
	}
	// 三个已知样式都必须被接受，否则统一入口的分派会在运行期才发现问题。
	for _, style := range []byte{
		currentClientNoticeStyleOverlay,
		currentClientNoticeStyleNativePrompt,
		currentClientNoticeStyleNativeWindow,
	} {
		if _, err := buildCurrentClientNoticeStyledBody(
			"x", currentClientNoticeDefaultColor, style,
		); err != nil {
			t.Fatalf("style %d was rejected: %v", style, err)
		}
	}
}

// 统一入口：native_window 这个 surface 必须写出样式 2 的信封。
func TestSendNoticeRoutesTheNativeWindowSurface(t *testing.T) {
	service, session, connection := newNoticeRoutingTest()
	if err := service.sendNotice(session, noticeRequest{
		Surface: noticeSurfaceNativeWindow,
		Text:    "服务端统一入口",
		Source:  "unit_test",
	}); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != currentClientNoticeMsgID ||
		packet.Header.Classification != 0 || len(trailing) != 0 {
		t.Fatalf("native window notice header=%+v trailing=%x", packet.Header, trailing)
	}
	if packet.Body[0] != currentClientNoticeType ||
		packet.Body[1] != currentClientNoticeVersionStyled ||
		packet.Body[8] != currentClientNoticeStyleNativeWindow {
		t.Fatalf("native window notice body head=%x", packet.Body[:10])
	}
}

// 覆盖层那个 surface 不能被这次改动带走：它仍然是版本 1。
func TestSendNoticeKeepsTheOverlaySurfaceOnVersionOne(t *testing.T) {
	service, session, connection := newNoticeRoutingTest()
	if err := service.sendNotice(session, noticeRequest{
		Surface: noticeSurfaceOverlay,
		Text:    "覆盖层",
		Color:   0xFF445566,
		Source:  "unit_test",
	}); err != nil {
		t.Fatal(err)
	}
	packet, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Body[1] != currentClientNoticeVersion {
		t.Fatalf("overlay surface version=%d want %d",
			packet.Body[1], currentClientNoticeVersion)
	}
}
