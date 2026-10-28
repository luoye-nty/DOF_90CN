package dnfrobot

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"

	dnfrobotdomain "longheng.io/server/internal/modules/dnf/robot"
)

var (
	ErrNotReady     = errors.New("dnf robot is not ready")
	ErrBindRejected = errors.New("dnf robot session bind was rejected")
)

type State string

const (
	StateStopped      State = "stopped"
	StateConnecting   State = "connecting"
	StateBinding      State = "binding"
	StateSelecting    State = "selecting"
	StateTownReady    State = "town_ready"
	StateStoreOpening State = "store_opening"
	StateStoreReady   State = "store_ready"
	StateStoreClosing State = "store_closing"
	StateError        State = "error"
)

const (
	privateStoreCreateOpcode       uint16 = 85
	privateStoreCloseOpcode        uint16 = 86
	privateStoreCompleteOpcode     uint16 = 87
	privateStoreMaximumRows               = 14
	privateStoreDisplayRetryGap           = 2 * time.Second
	privateStoreDisplayMaxAttempts        = 4
)

type PrivateStoreRow struct {
	DisplayIndex uint16 `json:"display_index"`
	Price        uint32 `json:"price"`
	ListType     uint8  `json:"list_type"`
	Slot         int16  `json:"slot"`
	WireValue    uint32 `json:"wire_value"`
}

type pendingPrivateStore struct {
	title []byte
	rows  []PrivateStoreRow
}

type ClientSnapshot struct {
	State       State     `json:"state"`
	Endpoint    string    `json:"endpoint,omitempty"`
	CharacterID uint16    `json:"character_id"`
	Slot        uint8     `json:"slot,omitempty"`
	Village     uint8     `json:"village"`
	Area        uint8     `json:"area"`
	X           uint16    `json:"x"`
	Y           uint16    `json:"y"`
	ReadyAt     time.Time `json:"ready_at,omitempty"`
	StateSince  time.Time `json:"state_since,omitempty"`
	LastReceive time.Time `json:"last_receive_at,omitempty"`
	LastSend    time.Time `json:"last_send_at,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
}

type Client struct {
	profile  dnfrobotdomain.Profile
	endpoint string
	secret   []byte

	mu            sync.RWMutex
	state         State
	conn          net.Conn
	selectedID    uint16
	selectedSlot  uint8
	village       uint8
	area          uint8
	x             uint16
	y             uint16
	readyAt       time.Time
	stateSince    time.Time
	lastReceive   time.Time
	lastSend      time.Time
	lastError     string
	progressSent  bool
	selectionSent bool
	townReadySent bool
	pendingStore  *pendingPrivateStore
	storeActive   bool
	storeCreated  bool
	storeAttempts int
	storeRetryAt  time.Time

	sendMu    sync.Mutex
	sequence  uint16
	ready     chan struct{}
	readyOnce sync.Once
	readErr   chan error
	closeOnce sync.Once
}

func NewClient(profile dnfrobotdomain.Profile, endpoint string, secret []byte) *Client {
	return &Client{
		profile:    profile,
		endpoint:   endpoint,
		secret:     append([]byte(nil), secret...),
		state:      StateStopped,
		stateSince: time.Now().UTC(),
		village:    profile.Village,
		area:       profile.Area,
		x:          profile.X,
		y:          profile.Y,
		ready:      make(chan struct{}),
		readErr:    make(chan error, 1),
	}
}

func (c *Client) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.setState(StateConnecting, nil)
	conn, err := (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", c.endpoint)
	if err != nil {
		c.setState(StateError, err)
		return err
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer c.Close()

	go c.readLoop(conn)
	c.setState(StateBinding, nil)
	bindBody, err := buildBindBody(c.secret, time.Now(), c.profile.CharacterID, c.profile.AccountID)
	if err != nil {
		c.setState(StateError, err)
		return err
	}
	if err := c.send(robotBindOpcode, bindBody); err != nil {
		c.setState(StateError, err)
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-c.readErr:
		c.setState(StateError, err)
		return err
	case <-c.ready:
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-c.readErr:
		c.setState(StateError, err)
		return err
	}
}

func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.mu.Lock()
		conn := c.conn
		c.conn = nil
		c.setStateLocked(StateStopped, nil)
		c.mu.Unlock()
		if conn != nil {
			err = conn.Close()
		}
	})
	return err
}

func (c *Client) Snapshot() ClientSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return ClientSnapshot{
		State: c.state, Endpoint: c.endpoint, CharacterID: c.selectedID,
		Slot: c.selectedSlot, Village: c.village, Area: c.area, X: c.x, Y: c.y,
		ReadyAt: c.readyAt, StateSince: c.stateSince, LastReceive: c.lastReceive, LastSend: c.lastSend, LastError: c.lastError,
	}
}

func (c *Client) Move(village, area uint8, x, y uint16, moveType uint8) error {
	c.mu.RLock()
	ready := c.state == StateTownReady
	oldVillage, oldArea := c.village, c.area
	c.mu.RUnlock()
	if !ready {
		return ErrNotReady
	}
	if village != oldVillage || area != oldArea {
		body := make([]byte, 16)
		body[0], body[1] = village, area
		binary.LittleEndian.PutUint16(body[2:4], x)
		binary.LittleEndian.PutUint16(body[4:6], y)
		body[6] = moveType
		binary.LittleEndian.PutUint16(body[7:9], uint16(oldVillage))
		binary.LittleEndian.PutUint16(body[9:11], uint16(oldArea))
		if err := c.send(36, body); err != nil {
			return err
		}
	}
	body := make([]byte, 7)
	binary.LittleEndian.PutUint16(body[0:2], x)
	binary.LittleEndian.PutUint16(body[2:4], y)
	body[4] = moveType
	binary.LittleEndian.PutUint16(body[5:7], 100)
	if err := c.send(35, body); err != nil {
		return err
	}
	c.mu.Lock()
	c.village, c.area, c.x, c.y = village, area, x, y
	c.mu.Unlock()
	return nil
}

func (c *Client) Chat(mode uint8, message string) error {
	return c.chat(mode, 0, message)
}

// Megaphone sends the current client's global mode-11 chat request. The
// argument is the exact main-inventory slot of item 36 and is consumed by the
// server's normal megaphone owner before broadcasting.
func (c *Client) Megaphone(slot int16, message string) error {
	if slot < 0 {
		return fmt.Errorf("invalid dnf robot megaphone slot")
	}
	return c.chat(11, uint32(uint16(slot)), message)
}

func (c *Client) chat(mode uint8, argument uint32, message string) error {
	c.mu.RLock()
	ready := c.state == StateTownReady
	c.mu.RUnlock()
	if !ready {
		return ErrNotReady
	}
	encoded, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(message))
	if err != nil || len(encoded) == 0 || len(encoded) > 512 {
		return fmt.Errorf("invalid dnf robot chat message")
	}
	if mode == 0 {
		mode = 3
	}
	body := make([]byte, 11+len(encoded))
	body[0] = mode
	binary.LittleEndian.PutUint32(body[3:7], argument)
	binary.LittleEndian.PutUint32(body[7:11], uint32(len(encoded)))
	copy(body[11:], encoded)
	return c.send(17, body)
}

func (c *Client) OpenPrivateStore(title string, rows []PrivateStoreRow) error {
	c.mu.Lock()
	if c.state != StateTownReady || c.pendingStore != nil || c.storeActive {
		c.mu.Unlock()
		return ErrNotReady
	}
	encodedTitle, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(title))
	if err != nil || len(encodedTitle) == 0 || len(encodedTitle) > 80 || len(rows) == 0 || len(rows) > privateStoreMaximumRows {
		c.mu.Unlock()
		return errors.New("invalid dnf robot private store")
	}
	seenDisplay := make(map[uint16]struct{}, len(rows))
	seenSlot := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		key := fmt.Sprintf("%d:%d", row.ListType, row.Slot)
		if row.DisplayIndex >= privateStoreMaximumRows || row.Price == 0 || row.WireValue == 0 || row.Slot < 0 || (row.ListType != 0 && row.ListType != 1) {
			c.mu.Unlock()
			return errors.New("invalid dnf robot private store row")
		}
		if _, exists := seenDisplay[row.DisplayIndex]; exists {
			c.mu.Unlock()
			return errors.New("duplicate dnf robot private store display row")
		}
		if _, exists := seenSlot[key]; exists {
			c.mu.Unlock()
			return errors.New("duplicate dnf robot private store inventory row")
		}
		seenDisplay[row.DisplayIndex] = struct{}{}
		seenSlot[key] = struct{}{}
	}
	c.pendingStore = &pendingPrivateStore{title: append([]byte(nil), encodedTitle...), rows: append([]PrivateStoreRow(nil), rows...)}
	c.storeCreated = false
	c.storeAttempts = 0
	c.storeRetryAt = time.Time{}
	c.setStateLocked(StateStoreOpening, nil)
	village, area, x, y := c.village, c.area, c.x, c.y
	c.mu.Unlock()
	body := make([]byte, 8)
	body[0], body[1] = village, area
	binary.LittleEndian.PutUint16(body[2:4], x)
	binary.LittleEndian.PutUint16(body[4:6], y)
	binary.LittleEndian.PutUint16(body[6:8], 0xffff)
	if err := c.send(privateStoreCreateOpcode, body); err != nil {
		c.resetPrivateStore(err)
		return err
	}
	return nil
}

func (c *Client) ClosePrivateStore() error {
	c.mu.Lock()
	if !c.storeActive || c.state != StateStoreReady {
		c.mu.Unlock()
		return ErrNotReady
	}
	c.setStateLocked(StateStoreClosing, nil)
	c.mu.Unlock()
	if err := c.send(privateStoreCloseOpcode, nil); err != nil {
		c.mu.Lock()
		if c.state == StateStoreClosing {
			c.setStateLocked(StateStoreReady, err)
		}
		c.mu.Unlock()
		return err
	}
	return nil
}

func (c *Client) readLoop(conn net.Conn) {
	buffer := make([]byte, 0, 8192)
	chunk := make([]byte, 8192)
	for {
		n, err := conn.Read(chunk)
		if n > 0 {
			buffer = append(buffer, chunk[:n]...)
			for len(buffer) >= 4 {
				size, outer, ready, valid := nextFrame(buffer)
				if !valid {
					c.signalReadError(errors.New("invalid dnf robot server frame"))
					return
				}
				if !ready {
					break
				}
				packets, decodeErr := decodeFrame(buffer[:size], outer)
				buffer = append(buffer[:0], buffer[size:]...)
				if decodeErr != nil {
					c.signalReadError(decodeErr)
					return
				}
				for _, message := range packets {
					if handleErr := c.handle(message); handleErr != nil {
						c.signalReadError(handleErr)
						return
					}
				}
			}
			if len(buffer) > maximumFrameSize {
				c.signalReadError(errors.New("dnf robot receive buffer exceeded limit"))
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				c.signalReadError(err)
			} else {
				c.signalReadError(io.EOF)
			}
			return
		}
	}
}

func (c *Client) handle(message packet) error {
	c.mu.Lock()
	c.lastReceive = time.Now().UTC()
	c.mu.Unlock()
	switch message.Opcode {
	case robotBindOpcode:
		if len(message.Body) != 4 || message.Body[0] != 1 {
			code := byte(0)
			if len(message.Body) > 1 {
				code = message.Body[1]
			}
			return fmt.Errorf("%w: code %d", ErrBindRejected, code)
		}
		c.mu.Lock()
		c.selectedSlot = message.Body[1]
		c.selectedID = binary.LittleEndian.Uint16(message.Body[2:4])
		c.setStateLocked(StateSelecting, nil)
		c.mu.Unlock()
		return c.send(8, []byte{0xff, 0xff, 0x02})
	case 2:
		// The current server answers GET_USERINFO with the class-0/op2
		// character roster.  The authenticated bind already froze the exact
		// character and slot, so the robot only needs this roster boundary
		// before sending SELECT_CHARACTER.  Older test/reference servers used
		// op8 here; keep that path below for compatibility.
		if message.Classification != 0 {
			return nil
		}
		return c.sendSelectedCharacterOnce()
	case 8:
		return c.sendSelectedCharacterOnce()
	case 4:
		c.mu.Lock()
		if c.progressSent {
			c.mu.Unlock()
			return nil
		}
		c.progressSent = true
		c.mu.Unlock()
		if err := c.send(143, []byte{0, 36, 0, 0, 0, 1}); err != nil {
			c.mu.Lock()
			c.progressSent = false
			c.mu.Unlock()
			return err
		}
	case 24:
		c.mu.RLock()
		alreadyReady := c.townReadySent
		c.mu.RUnlock()
		if alreadyReady {
			return nil
		}
		if err := c.send(1345, []byte{2, 0, 0, 0}); err != nil {
			return err
		}
		area := make([]byte, 16)
		area[0], area[1] = c.profile.Village, c.profile.Area
		binary.LittleEndian.PutUint16(area[2:4], c.profile.X)
		binary.LittleEndian.PutUint16(area[4:6], c.profile.Y)
		if err := c.send(36, area); err != nil {
			return err
		}
		position := make([]byte, 7)
		binary.LittleEndian.PutUint16(position[0:2], c.profile.X)
		binary.LittleEndian.PutUint16(position[2:4], c.profile.Y)
		position[4] = c.profile.MoveType
		binary.LittleEndian.PutUint16(position[5:7], 100)
		if err := c.send(35, position); err != nil {
			return err
		}
		c.mu.Lock()
		c.townReadySent = true
		c.setStateLocked(StateTownReady, nil)
		c.readyAt = time.Now().UTC()
		c.lastError = ""
		c.mu.Unlock()
		c.readyOnce.Do(func() { close(c.ready) })
	case privateStoreCreateOpcode:
		if len(message.Body) == 0 || message.Body[0] != 1 {
			c.resetPrivateStore(errors.New("dnf robot private store creation was rejected"))
			return nil
		}
		c.mu.Lock()
		c.storeCreated = true
		c.mu.Unlock()
		_, err := c.retryPrivateStoreDisplay(time.Now().UTC(), true)
		return err
	case privateStoreCompleteOpcode:
		if len(message.Body) == 0 || message.Body[0] != 1 {
			c.resetPrivateStore(errors.New("dnf robot private store contents were rejected"))
			return nil
		}
		c.mu.Lock()
		c.pendingStore = nil
		c.storeActive = true
		c.storeCreated = false
		c.storeAttempts = 0
		c.storeRetryAt = time.Time{}
		c.setStateLocked(StateStoreReady, nil)
		c.lastError = ""
		c.mu.Unlock()
	case privateStoreCloseOpcode:
		if len(message.Body) != 0 && message.Body[0] == 0 {
			c.mu.Lock()
			c.setStateLocked(StateStoreReady, errors.New("dnf robot private store close was rejected"))
			c.mu.Unlock()
			return nil
		}
		c.mu.Lock()
		c.pendingStore = nil
		c.storeActive = false
		c.storeCreated = false
		c.storeAttempts = 0
		c.storeRetryAt = time.Time{}
		c.setStateLocked(StateTownReady, nil)
		c.mu.Unlock()
	}
	return nil
}

func (c *Client) sendSelectedCharacterOnce() error {
	c.mu.Lock()
	if c.selectionSent {
		c.mu.Unlock()
		return nil
	}
	selectedID, slot := c.selectedID, c.selectedSlot
	c.selectionSent = true
	c.mu.Unlock()

	body := make([]byte, 11)
	binary.LittleEndian.PutUint32(body[0:4], uint32(selectedID))
	body[4] = slot
	if err := c.send(4, body); err != nil {
		c.mu.Lock()
		c.selectionSent = false
		c.mu.Unlock()
		return err
	}
	return nil
}

func clonePendingPrivateStore(pending *pendingPrivateStore) *pendingPrivateStore {
	if pending == nil {
		return nil
	}
	return &pendingPrivateStore{title: append([]byte(nil), pending.title...), rows: append([]PrivateStoreRow(nil), pending.rows...)}
}

func buildPrivateStoreCompleteBody(pending *pendingPrivateStore) []byte {
	if pending == nil {
		return nil
	}
	body := make([]byte, 4+len(pending.title)+1+len(pending.rows)*13+2)
	binary.LittleEndian.PutUint32(body[0:4], uint32(len(pending.title)))
	copy(body[4:], pending.title)
	offset := 4 + len(pending.title)
	body[offset] = byte(len(pending.rows))
	offset++
	for _, row := range pending.rows {
		binary.LittleEndian.PutUint16(body[offset:offset+2], row.DisplayIndex)
		binary.LittleEndian.PutUint32(body[offset+2:offset+6], row.Price)
		body[offset+6] = row.ListType
		binary.LittleEndian.PutUint16(body[offset+7:offset+9], uint16(row.Slot))
		binary.LittleEndian.PutUint32(body[offset+9:offset+13], row.WireValue)
		offset += 13
	}
	binary.LittleEndian.PutUint16(body[offset:offset+2], 0xffff)
	return body
}

// retryPrivateStoreDisplay mirrors the recovered 20260825 runtime's bounded
// CompletePrivateStoreDisplay retry.  The create acknowledgement is the
// irreversible boundary: only after it arrives may op87 be retried, at most
// four total attempts and no faster than once every two seconds.
func (c *Client) retryPrivateStoreDisplay(now time.Time, immediate bool) (bool, error) {
	if c == nil {
		return false, ErrNotReady
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	c.mu.Lock()
	if c.state != StateStoreOpening || !c.storeCreated || c.pendingStore == nil {
		c.mu.Unlock()
		return false, nil
	}
	if !immediate && !c.storeRetryAt.IsZero() && now.Before(c.storeRetryAt) {
		c.mu.Unlock()
		return false, nil
	}
	if c.storeAttempts >= privateStoreDisplayMaxAttempts {
		c.mu.Unlock()
		return false, errors.New("dnf robot private store display acknowledgement timed out")
	}
	pending := clonePendingPrivateStore(c.pendingStore)
	c.storeAttempts++
	c.storeRetryAt = now.Add(privateStoreDisplayRetryGap)
	c.mu.Unlock()

	if err := c.send(privateStoreCompleteOpcode, buildPrivateStoreCompleteBody(pending)); err != nil {
		c.resetPrivateStore(err)
		return false, err
	}
	return true, nil
}

func (c *Client) resetPrivateStore(err error) {
	c.mu.Lock()
	c.pendingStore = nil
	c.storeActive = false
	c.storeCreated = false
	c.storeAttempts = 0
	c.storeRetryAt = time.Time{}
	if c.state != StateStopped {
		c.setStateLocked(StateTownReady, err)
	}
	if err != nil && c.state == StateStopped {
		c.lastError = err.Error()
	}
	c.mu.Unlock()
}

func (c *Client) send(opcode uint16, body []byte) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return net.ErrClosed
	}
	frame, err := buildPacket(opcode, c.sequence, body)
	if err != nil {
		return err
	}
	if _, err := conn.Write(frame); err != nil {
		return err
	}
	c.mu.Lock()
	c.lastSend = time.Now().UTC()
	c.mu.Unlock()
	c.sequence++
	return nil
}

func (c *Client) signalReadError(err error) {
	select {
	case c.readErr <- err:
	default:
	}
}

func (c *Client) setState(state State, err error) {
	c.mu.Lock()
	c.setStateLocked(state, err)
	c.mu.Unlock()
}

func (c *Client) setStateLocked(state State, err error) {
	if c.state != state {
		c.state = state
		c.stateSince = time.Now().UTC()
	}
	if err != nil {
		c.lastError = err.Error()
	} else if state == StateTownReady || state == StateStoreReady {
		c.lastError = ""
	}
}

func (c *Client) pendingOperationTimedOut(now time.Time, timeout time.Duration) (State, bool) {
	if c == nil || timeout <= 0 {
		return StateStopped, false
	}
	c.mu.RLock()
	state, since := c.state, c.stateSince
	c.mu.RUnlock()
	switch state {
	case StateConnecting, StateBinding, StateSelecting, StateStoreOpening, StateStoreClosing:
		return state, !since.IsZero() && now.Sub(since) >= timeout
	default:
		return state, false
	}
}
