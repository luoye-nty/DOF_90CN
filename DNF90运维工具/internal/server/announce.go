// 全服公告广播 hub：长轮询 handler 订阅，公告发布时非阻塞投递。
package server

import "sync"

// AnnounceHub 维护所有等待中的长轮询订阅连接。
type AnnounceHub struct {
	mu    sync.Mutex
	conns map[chan []byte]struct{}
}

// NewAnnounceHub 构造公告广播 hub。
func NewAnnounceHub() *AnnounceHub {
	return &AnnounceHub{conns: make(map[chan []byte]struct{})}
}

// Subscribe 注册一个订阅，返回接收 channel 与退订函数。
func (h *AnnounceHub) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 8)
	h.mu.Lock()
	h.conns[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.conns, ch)
		h.mu.Unlock()
	}
}

// Broadcast 向所有订阅者非阻塞投递 payload，返回成功投递数量。
// 慢客户端（channel 满）会被跳过，符合「不补发」语义。
func (h *AnnounceHub) Broadcast(payload []byte) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for ch := range h.conns {
		select {
		case ch <- payload:
			n++
		default:
		}
	}
	return n
}
