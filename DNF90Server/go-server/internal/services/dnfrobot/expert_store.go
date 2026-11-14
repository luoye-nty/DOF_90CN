package dnfrobot

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
)

const (
	expertStoreCreateOpcode      uint16 = 598
	expertStoreCloseOpcode       uint16 = 600
	expertStoreCloseNotification uint16 = 539
	expertStoreAckTimeout               = 8 * time.Second
	expertStoreDisjointWireKind  byte   = 0
	expertStoreEnchantWireKind   byte   = 3
)

// OpenExpertJobStore creates one decomposer or enchanter stall and waits for
// the class-1/op598 acknowledgement. The character profession must already be
// prepared; the server remains authoritative for profession, cost and
// endurance validation.
func (c *Client) OpenExpertJobStore(ctx context.Context, cost uint32) error {
	if c == nil {
		return ErrNotReady
	}
	if ctx == nil {
		ctx = context.Background()
	}
	kind, wireKind, name, err := expertStoreDefinition(c.profile.ExpertStoreKind)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.state != StateTownReady || c.storeActive || c.expertStoreActive || c.pendingStore != nil {
		c.mu.Unlock()
		return ErrNotReady
	}
	x, y := c.x, c.y
	body, err := buildExpertStoreCreateBody(wireKind, name, cost, int16(x), int16(y), c.selectedID)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	ack := make(chan error, 1)
	c.expertStoreAck = ack
	c.expertStoreKind = kind
	c.setStateLocked(StateStoreOpening, nil)
	c.mu.Unlock()

	if err := c.send(expertStoreCreateOpcode, body); err != nil {
		c.resetExpertStore(err)
		return err
	}
	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		c.resetExpertStore(ctx.Err())
		return ctx.Err()
	case <-time.After(expertStoreAckTimeout):
		err := errors.New("dnf robot expert store opening timed out")
		c.resetExpertStore(err)
		return err
	}
}

func buildExpertStoreCreateBody(wireKind byte, name []byte, cost uint32, x, y int16, link uint16) ([]byte, error) {
	if wireKind != expertStoreDisjointWireKind && wireKind != expertStoreEnchantWireKind {
		return nil, fmt.Errorf("unsupported expert store wire kind %d", wireKind)
	}
	if len(name) == 0 || len(name) > 255 {
		return nil, fmt.Errorf("invalid expert store name length %d", len(name))
	}
	body := make([]byte, 15+len(name))
	body[0] = wireKind
	binary.LittleEndian.PutUint32(body[1:5], uint32(len(name)))
	copy(body[5:], name)
	offset := 5 + len(name)
	binary.LittleEndian.PutUint32(body[offset:offset+4], cost)
	binary.LittleEndian.PutUint16(body[offset+4:offset+6], uint16(x))
	binary.LittleEndian.PutUint16(body[offset+6:offset+8], uint16(y))
	binary.LittleEndian.PutUint16(body[offset+8:offset+10], link)
	return body, nil
}

// CloseExpertJobStore closes the active expert stall. The current server
// broadcasts NOTI539 instead of sending a class-1/op600 acknowledgement.
func (c *Client) CloseExpertJobStore(ctx context.Context) error {
	if c == nil {
		return ErrNotReady
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if c.state != StateStoreReady || !c.expertStoreActive {
		c.mu.Unlock()
		return ErrNotReady
	}
	ack := make(chan error, 1)
	c.expertCloseAck = ack
	c.setStateLocked(StateStoreClosing, nil)
	c.mu.Unlock()
	if err := c.send(expertStoreCloseOpcode, nil); err != nil {
		c.resetExpertStore(err)
		return err
	}
	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(expertStoreAckTimeout):
		return errors.New("dnf robot expert store closing timed out")
	}
}

func expertStoreDefinition(kind string) (string, byte, []byte, error) {
	switch kind {
	case "disjoint":
		name, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte("分解机"))
		return kind, expertStoreDisjointWireKind, name, err
	case "enchant":
		name, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte("附魔机"))
		return kind, expertStoreEnchantWireKind, name, err
	default:
		return "", 0, nil, fmt.Errorf("unsupported expert store kind %q", kind)
	}
}

func (c *Client) resetExpertStore(err error) {
	c.mu.Lock()
	c.expertStoreAck = nil
	c.expertCloseAck = nil
	c.expertStoreKind = ""
	c.expertStoreActive = false
	if c.state != StateStopped {
		c.setStateLocked(StateTownReady, err)
	}
	c.mu.Unlock()
}
