package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"testing"

	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

func TestSendNoticeRoutesEveryNativeSurfaceToItsOwnTypeByte(t *testing.T) {
	for _, surface := range []noticeSurface{
		noticeSurfaceSystemTab, noticeSurfaceSystemMagenta, noticeSurfaceSystemCyan,
		noticeSurfaceSystemLuckyTemplate, noticeSurfaceMegaphone, noticeSurfaceNormalTab,
	} {
		service, session, connection := newNoticeRoutingTest()
		if err := service.sendNotice(session, noticeRequest{
			Surface: surface, Text: "转职提示", Source: "unit_test",
		}); err != nil {
			t.Fatalf("%s: %v", surface.Name, err)
		}
		packet, trailing := splitGameServerUpperPacketWithHeader(
			t, connection.write.Bytes(), dnfproto.GameServerUpperHeaderSize16,
		)
		if packet.Header.MsgID != currentNativeNoticeMsgID ||
			packet.Header.Classification != 0 || len(trailing) != 0 {
			t.Fatalf("%s header=%+v trailing=%x", surface.Name, packet.Header, trailing)
		}
		if packet.Body[0] != byte(surface.Type) {
			t.Fatalf("%s type byte=%d want %d", surface.Name, packet.Body[0], surface.Type)
		}
	}
}

func TestSendNoticeOverlaySurfaceCarriesTheRequestedColor(t *testing.T) {
	service, session, connection := newNoticeRoutingTest()
	const color = uint32(0xFF33CCFF)
	if err := service.sendNotice(session, noticeRequest{
		Surface: noticeSurfaceOverlay, Text: "转职提示", Color: color, Source: "unit_test",
	}); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != currentClientNoticeMsgID || len(trailing) != 0 {
		t.Fatalf("overlay header=%+v trailing=%x", packet.Header, trailing)
	}
	want, err := buildCurrentClientNoticeBody("转职提示", color)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packet.Body, want) {
		t.Fatalf("overlay body=%x want=%x", packet.Body, want)
	}
	if got := binary.LittleEndian.Uint32(packet.Body[4:8]); got != color {
		t.Fatalf("overlay color=%08x want %08x", got, color)
	}
	// The overlay must not land on the type the mood projection owns, or
	// TryConsumePrivateMoodProjection would claim it before the notice
	// consumer ever runs.
	if packet.Body[0] != currentClientNoticeType ||
		packet.Body[0] == currentMoodProjectionType {
		t.Fatalf("overlay private type byte=%d", packet.Body[0])
	}
}

func TestSendNoticeNativeSurfaceIgnoresARequestedColorWithoutChangingTheWire(t *testing.T) {
	service, session, connection := newNoticeRoutingTest()
	if err := service.sendNotice(session, noticeRequest{
		Surface: noticeSurfaceSystemTab, Text: "转职提示", Color: 0xFF00FF00, Source: "unit_test",
	}); err != nil {
		t.Fatal(err)
	}
	want, err := buildCurrentNativeNoticeBody(noticeSurfaceSystemTab.Type, 0, 0, "转职提示")
	if err != nil {
		t.Fatal(err)
	}
	packet, _ := splitGameServerUpperPacketWithHeader(
		t, connection.write.Bytes(), dnfproto.GameServerUpperHeaderSize16,
	)
	if !bytes.Equal(packet.Body, want) {
		t.Fatalf("native body=%x want=%x", packet.Body, want)
	}
}

func TestSendNoticeChannelOverrideReachesTheWire(t *testing.T) {
	service, session, connection := newNoticeRoutingTest()
	if err := service.sendNotice(session, noticeRequest{
		Surface: noticeSurfaceSystemTab, Text: "转职提示", Channel: 7, Source: "unit_test",
	}); err != nil {
		t.Fatal(err)
	}
	packet, _ := splitGameServerUpperPacketWithHeader(
		t, connection.write.Bytes(), dnfproto.GameServerUpperHeaderSize16,
	)
	// Third field is the handle byte sub_2327B30 loads from [esi+0x1C].
	if packet.Body[3] != 7 {
		t.Fatalf("native notice channel byte=%d want 7", packet.Body[3])
	}
}

func TestSendNoticeDropsAnUnusableRequestWithoutWriting(t *testing.T) {
	for _, surface := range []noticeSurface{noticeSurfaceSystemTab, noticeSurfaceOverlay} {
		service, session, connection := newNoticeRoutingTest()
		if err := service.sendNotice(session, noticeRequest{
			Surface: surface, Text: "", Source: "unit_test",
		}); err != nil {
			t.Fatalf("%s: %v", surface.Name, err)
		}
		if got := connection.write.Bytes(); len(got) != 0 {
			t.Fatalf("%s wrote=%x for an empty text", surface.Name, got)
		}
	}
}

func TestSendNoticeRejectsAnUnknownBackend(t *testing.T) {
	service, session, connection := newNoticeRoutingTest()
	err := service.sendNotice(session, noticeRequest{
		Surface: noticeSurface{Name: "invented", Backend: noticeBackend(9)},
		Text:    "转职提示",
		Source:  "unit_test",
	})
	if err == nil {
		t.Fatal("unknown notice backend was accepted")
	}
	if got := connection.write.Bytes(); len(got) != 0 {
		t.Fatalf("unknown notice backend wrote=%x", got)
	}
}
