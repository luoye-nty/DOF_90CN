package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/services/dnfrobot"
)

var errRobotPrivateStoreInventoryEmpty = errors.New("dnf robot has no private-store-eligible inventory")

// RobotGameEndpoint returns the exact local endpoint for a catalog channel.
// It exposes no secret and does not bypass the real game listener; the robot
// still authenticates through the private HMAC bind and all normal owners.
func (s *Service) RobotGameEndpoint(channelID int) (string, error) {
	if s == nil || channelID <= 0 {
		return "", errors.New("dnf robot channel is invalid")
	}
	s.mu.Lock()
	catalog := s.catalog
	host := strings.TrimSpace(s.options.gameListenHost)
	s.mu.Unlock()
	if catalog == nil {
		return "", errors.New("dnf game channel catalog is not ready")
	}
	channel, ok := catalog.Channel(channelID)
	if !ok || channel.Port <= 0 {
		return "", errors.New("dnf robot channel is not available")
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(channel.Port)), nil
}

// WaitRobotGameEndpointStable requires consecutive successful TCP accepts
// before the automatic fleet scheduler may start.  A single successful bind
// during process startup is not enough: the listener must remain reachable
// across the whole sample window.
func (s *Service) WaitRobotGameEndpointStable(ctx context.Context, channelID int, samples int, interval time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if samples <= 0 {
		samples = 1
	}
	if interval < 0 {
		return errors.New("dnf robot gateway stability interval is invalid")
	}
	endpoint, err := s.RobotGameEndpoint(channelID)
	if err != nil {
		return err
	}
	for sample := 0; sample < samples; sample++ {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, dialErr := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(probeCtx, "tcp", endpoint)
		cancel()
		if dialErr != nil {
			return fmt.Errorf("probe dnf robot game endpoint %s sample=%d/%d: %w", endpoint, sample+1, samples, dialErr)
		}
		_ = conn.Close()
		if sample+1 == samples || interval == 0 {
			continue
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

// RobotPrivateStoreRows resolves the robot's current inventory through the
// same PVF eligibility and instance rules used by the private-store owner.
// This keeps the protocol client from guessing whether a u32 row value is a
// stack quantity or an equipment instance identity.
func (s *Service) RobotPrivateStoreRows(ctx context.Context, characterID uint16, maximumRows int, unitPrice uint32) ([]dnfrobot.PrivateStoreRow, error) {
	if s == nil || characterID == 0 || maximumRows <= 0 || maximumRows > currentPrivateStoreMaxRows || unitPrice == 0 {
		return nil, errors.New("invalid dnf robot private store plan")
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Inventory == nil {
		return nil, errors.New("dnf robot inventory repository is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	inventory, found, err := repositories.Inventory.Load(ctx, strconv.Itoa(int(characterID)))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("dnf robot inventory was not found")
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	type candidate struct {
		listType uint8
		slot     int16
		wire     uint32
	}
	candidates := make([]candidate, 0, len(inventory.Slots))
	now := time.Now()
	for key, stack := range inventory.Slots {
		parts := strings.Split(key, ":")
		if len(parts) != 2 {
			continue
		}
		listValue, listErr := strconv.ParseUint(parts[0], 10, 8)
		slotValue, slotErr := strconv.ParseInt(parts[1], 10, 16)
		if listErr != nil || slotErr != nil || (listValue != 0 && listValue != 1) || slotValue < 0 || stack.ItemID <= 0 || stack.Count <= 0 ||
			currentDungeonDiscardStackLocked(stack) || currentDungeonDiscardStackExpired(stack, now) {
			continue
		}
		definition, resolveErr := catalog.ResolveItem(uint32(stack.ItemID))
		if resolveErr != nil {
			continue
		}
		_, _, eligible := currentPrivateStoreRegistrationPolicyForDefinition(definition, stack)
		if !eligible {
			continue
		}
		wire := uint32(0)
		switch definition.Kind {
		case dungeonDropItemEquipment:
			wire = currentPrivateStoreEquipmentInstance(byte(listValue), stack)
		case dungeonDropItemStackable:
			if stack.Count > math.MaxUint32 {
				wire = math.MaxUint32
			} else {
				wire = uint32(stack.Count)
			}
		}
		if _, _, _, _, valid := s.resolveCurrentPrivateStoreRowStack(byte(listValue), stack, wire); !valid {
			continue
		}
		candidates = append(candidates, candidate{listType: uint8(listValue), slot: int16(slotValue), wire: wire})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].listType != candidates[j].listType {
			return candidates[i].listType < candidates[j].listType
		}
		return candidates[i].slot < candidates[j].slot
	})
	if len(candidates) > maximumRows {
		candidates = candidates[:maximumRows]
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: character=%d", errRobotPrivateStoreInventoryEmpty, characterID)
	}
	rows := make([]dnfrobot.PrivateStoreRow, len(candidates))
	for index, item := range candidates {
		rows[index] = dnfrobot.PrivateStoreRow{
			DisplayIndex: uint16(index), Price: unitPrice, ListType: item.listType, Slot: item.slot, WireValue: item.wire,
		}
	}
	return rows, nil
}
