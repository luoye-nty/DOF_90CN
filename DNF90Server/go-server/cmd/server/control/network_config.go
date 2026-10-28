package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

func (c *controller) network(_ context.Context, args []string) error {
	if len(args) == 0 || !strings.EqualFold(strings.TrimSpace(args[0]), "set") {
		return errors.New("network subcommand is required: set")
	}
	fs := newFlagSet("network set", c.stderr)
	advertiseIP := fs.String("advertise-ip", "", "RFC1918 address assigned to this server")
	regionID := fs.Int("region-id", 0, "optional region id (1-249)")
	regionName := fs.String("region-name", "", "optional region display name")
	if err := parseFlags(fs, args[1:]); err != nil {
		return err
	}
	return c.configureNetwork(*advertiseIP, *regionID, *regionName)
}

func (c *controller) configureNetwork(advertiseIP string, regionID int, regionName string) error {
	advertiseIP = strings.TrimSpace(advertiseIP)
	ip := net.ParseIP(advertiseIP)
	if ip == nil || ip.To4() == nil || !ip.IsPrivate() || ip.IsLoopback() {
		return errors.New("advertise-ip must be a non-loopback RFC1918 IPv4 address, such as a WireGuard 10.x address")
	}
	current, err := loadInstance(c.paths)
	if err != nil {
		return err
	}
	if state, found, err := c.loadManagedProcess(current.InstallationID, true); err != nil {
		return err
	} else if found {
		return fmt.Errorf("DNF90 is running as PID %d; stop it before changing the network profile", state.PID)
	}
	raw, err := decodeInstance(c.paths.instance)
	if err != nil {
		return err
	}
	raw.Server.AdvertiseIP = advertiseIP
	if regionID != 0 {
		raw.Protocol.RegionID = regionID
		raw.Protocol.ChannelAdvertiseServerIndex = regionID
	}
	if strings.TrimSpace(regionName) != "" {
		raw.Protocol.RegionName = strings.TrimSpace(regionName)
	}
	if err := validateInstance(raw); err != nil {
		return err
	}
	data, err := marshalInstance(raw)
	if err != nil {
		return err
	}
	if err := writeFile(c.paths.instance, data, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "Private game address configured: %s (region %d %s)\n", advertiseIP, raw.Protocol.RegionID, raw.Protocol.RegionName)
	return nil
}
