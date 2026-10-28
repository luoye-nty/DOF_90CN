package dnfbridge

import (
	"encoding/hex"
	"strings"

	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

// S2C 回包二次加密的服务端接线（设计见 `服务端回包二次加密_客户端DLL解密_设计文档_20260930.md`）。
//
// 三个开关值：
//
//	off    —— 完全不变（连 challenge 都不带标记），这是默认值；
//	shadow —— 带**不带**标记、只记录"如果开了会启用哪条连接"，用来先证明协商链路；
//	on     —— challenge 带标记；客户端回 version 3 时才加密这条连接的回包。
//
// 关键安全性质：**服务端只在客户端明确回话（version 3）之后才加密**，
// 因此任何"没准备好解密"的一端（旧 DLL、本地直连、第三方客户端）都保持明文，
// 不会收到乱码——这正是"服务端沉默/乱码 = 客户端卡住"这条老故障形状的防线。

// gameS2CCipherMode 当前模式的归一化结果。
func (s *Service) gameS2CCipherMode() string {
	if s == nil {
		return gameS2CCipherOff
	}
	return normalizeGameS2CCipher(s.options.gameS2CCipher)
}

// gameS2CMasterKey 返回固化主密钥（可用 DNFBRIDGE_GAME_S2C_KEY_BASE64 覆盖，仅供联调/灰度）。
func (s *Service) gameS2CMasterKey() ([]byte, error) {
	if s != nil {
		if override := strings.TrimSpace(s.options.gameS2CMasterKeyBase64); override != "" {
			return dnfproto.ParseS2CMasterKey([]byte(override))
		}
	}
	return dnfproto.DefaultS2CMasterKey(), nil
}

// prepareGameS2CCipher 在**发 challenge 之前**就把这条连接的加解密器建好。
//
// 为什么顺序不能反：客户端一旦看到 challenge 里的标记就会回 version 3（= 我会解密）。
// 如果服务端到那时才发现"主密钥坏了 / 派生失败"而回落明文，客户端却已经在解密明文，
// 结果是全乱码。所以这里先建好，建不出来就**根本不发标记**——客户端不会确认，也就不会解密。
func (s *Service) prepareGameS2CCipher(session *gameSession, nonce []byte, logEvent func(string, ...any)) (*dnfproto.S2CCipher, bool) {
	if s == nil || logEvent == nil {
		return nil, false
	}
	mode := s.gameS2CCipherMode()
	if mode != gameS2CCipherOn {
		return nil, false
	}
	masterKey, err := s.gameS2CMasterKey()
	if err != nil {
		logEvent("game-s2c-cipher-skipped", "reason", "master-key-invalid", "error", err)
		return nil, false
	}
	cipher, err := dnfproto.NewS2CCipher(masterKey, nonce)
	if err != nil {
		logEvent("game-s2c-cipher-skipped", "reason", "derive-failed", "error", err)
		return nil, false
	}
	return cipher, true
}

// negotiateGameS2CCipher 在 LANA 握手之后决定这条连接是否启用回包加密。
//
// nonce 是服务端写进 challenge 的 16 字节随机数：客户端也拿得到同一段字节，
// 因此两端各自就能派生出同一把连接密钥，**不需要任何额外往返**。
// prepared 是发 challenge 之前就建好的加解密器（nil = 本次没请求加密）。
func (s *Service) negotiateGameS2CCipher(
	session *gameSession,
	frame launcherAuthFrame,
	prepared *dnfproto.S2CCipher,
	logEvent func(string, ...any),
) {
	if s == nil || session == nil || logEvent == nil {
		return
	}
	mode := s.gameS2CCipherMode()
	if mode == gameS2CCipherOff {
		return
	}
	requested := prepared != nil
	clientReady := frame.Version == launcherAuthFrameVersionS2C
	if mode == gameS2CCipherShadow {
		logEvent("game-s2c-cipher-shadow",
			"requested", requested,
			"client_frame_version", frame.Version,
			"client_ready", clientReady,
			"would_enable", requested && clientReady)
		return
	}
	if !requested {
		// 上面 prepare 阶段已经记过具体原因（主密钥坏了/派生失败），这里不再重复。
		return
	}
	if !clientReady {
		// 旧 90CN.dll（或不装 DLL 的客户端）：保持明文，这是设计上允许的结果。
		logEvent("game-s2c-cipher-skipped",
			"reason", "client-did-not-acknowledge",
			"client_frame_version", frame.Version)
		return
	}
	session.s2cCipher = prepared
	logEvent("game-s2c-cipher-enabled",
		"codec", dnfproto.S2CCipherCodecName,
		"nonce_prefix", hex.EncodeToString(prepared.NoncePrefix()))
}

// applyGameS2CBodyCipher 在协商成功的连接上就地加密 body，返回是否真的加密了。
//
// 调用点必须在**算 checksum 之前**（checksum 覆盖 seq+body，客户端按线上字节校验）：
// 顺序反了客户端会整包拒绝，表现同样是"卡住"。
func (s *Service) applyGameS2CBodyCipher(session *gameSession, body []byte, msgID uint16, seq uint16) bool {
	if s == nil || session == nil || len(body) == 0 {
		return false
	}
	cipher := session.s2cCipher
	if cipher == nil {
		return false
	}
	cipher.Apply(body, msgID, seq)
	return true
}
