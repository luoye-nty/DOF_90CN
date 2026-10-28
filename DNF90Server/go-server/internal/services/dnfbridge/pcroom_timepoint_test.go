package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// 原生 228「累计在线得好礼」面板的按钮只发这几种 2 字节动作体。
func TestParseCurrentPCRoomTimePointRequestAcceptsNativeButtonShapes(t *testing.T) {
	tests := []struct {
		name       string
		body       []byte
		wantDaily  byte
		wantPeriod int
		wantErr    bool
	}{
		{name: "pure refresh", body: []byte{0, 255}, wantDaily: 0, wantPeriod: -1},
		{name: "daily 1", body: []byte{1, 255}, wantDaily: 1, wantPeriod: -1},
		{name: "daily 2", body: []byte{2, 255}, wantDaily: 2, wantPeriod: -1},
		{name: "daily 3", body: []byte{4, 255}, wantDaily: 4, wantPeriod: -1},
		{name: "daily 4", body: []byte{8, 255}, wantDaily: 8, wantPeriod: -1},
		{name: "period 1", body: []byte{16, 0}, wantDaily: 0, wantPeriod: 0},
		{name: "period 4", body: []byte{16, 3}, wantDaily: 0, wantPeriod: 3},
		{name: "short body", body: []byte{1}, wantErr: true},
		{name: "unknown action", body: []byte{32, 255}, wantErr: true},
		{name: "period index out of range", body: []byte{16, 4}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			daily, period, err := parseCurrentPCRoomTimePointRequest(test.body)
			if test.wantErr {
				if err == nil {
					t.Fatalf("body=%x unexpectedly accepted as daily=%d period=%d", test.body, daily, period)
				}
				return
			}
			if err != nil {
				t.Fatalf("body=%x rejected: %v", test.body, err)
			}
			if daily != test.wantDaily || period != test.wantPeriod {
				t.Fatalf("body=%x parsed daily=%d period=%d want daily=%d period=%d",
					test.body, daily, period, test.wantDaily, test.wantPeriod)
			}
		})
	}
}

// 回归：cmd=1/type=632 必须被路由到 handleCurrentPCRoomTimePoint。
// 修复前它没有路由，客户端点"领取"只会换来服务端一条
// "dnfbridge unhandled latest game command type=632"，游戏里表现为点了没反应。
func TestHandleAlignedGameCommandRoutesPCRoomTimePointClaim(t *testing.T) {
	service := &Service{}
	session := &gameSession{connID: "pcroom-claim-route", conn: &bufferConn{}}
	handled, err := service.handleAlignedGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketGetPcroomTimePointItem),
		[]byte{2, 255},
	)
	if err != nil {
		t.Fatalf("pcroom claim route returned error: %v", err)
	}
	if !handled {
		t.Fatal("cmd=1/type=632 fell through to the unhandled-command branch")
	}
}

// 累计循环默认开启，可以用 DNF90_PCROOM_PERIOD_LOOP=off 关掉（各 Service 各自读一次）。
func TestPCRoomPeriodLoopEnabledDefaultsOnAndFollowsEnv(t *testing.T) {
	if !(&Service{}).pcroomPeriodLoopEnabled() {
		t.Fatal("period loop must default to on")
	}
	t.Setenv("DNF90_PCROOM_PERIOD_LOOP", "off")
	if (&Service{}).pcroomPeriodLoopEnabled() {
		t.Fatal("DNF90_PCROOM_PERIOD_LOOP=off must disable the period loop")
	}
	t.Setenv("DNF90_PCROOM_PERIOD_LOOP", "true")
	if !(&Service{}).pcroomPeriodLoopEnabled() {
		t.Fatal("explicit true must keep the period loop on")
	}
}
