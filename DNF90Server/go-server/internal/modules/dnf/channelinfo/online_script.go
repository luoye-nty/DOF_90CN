package channelinfo

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Build90CNOnlineScript derives the channel table downloaded by the 90CN
// client from the raw PVF channel_info.etc. Source names and groups remain
// authoritative; only the advertised server ID and proved online type
// projection may differ from the raw in-game catalog.
//
// 严格版：源表缺少引导频道时直接失败。只开少数频道（甚至只开特殊区域频道）的运维配置
// 请走 Build90CNOnlineScriptAllowingMissingBootstrap。
func Build90CNOnlineScript(data []byte, sourceServerID, advertiseServerID, bootstrapChannelID int) ([]byte, error) {
	script, bootstrapFound, err := build90CNOnlineScript(data, sourceServerID, advertiseServerID, bootstrapChannelID)
	if err != nil {
		return nil, err
	}
	if !bootstrapFound {
		return nil, fmt.Errorf(
			"channel script source server %d is missing bootstrap channel %d",
			sourceServerID,
			bootstrapChannelID,
		)
	}
	return script, nil
}

// Build90CNOnlineScriptAllowingMissingBootstrap 是 Build90CNOnlineScript 的宽松版：
// 源频道表里没有引导频道时不再失败，而是返回 bootstrapPresent=false，由调用方决定
// 是告警还是拒绝。
//
// 为什么需要它：引导频道这条硬校验来自官方 PVF 流程——那边的引导频道是 id 1 / type 2，
// 由 OnlineTypeFor90CN 映成线上的 type 11；而线上这份 channel_info.etc 里并没有 id 1，
// 于是 dnfenum.BootstrapChannelID 被定成 19，校验就变成了"表里必须有 19"。
// 实测（2026-09-30，1192 次客户端连接）客户端**从不**连 10019：它从 7001 拿目录后
// 直接从目录里挑端口连。所以这条约束对这条链路是化石约束，运维只开任意一个频道时
// 不该把服务端拦在门外。
func Build90CNOnlineScriptAllowingMissingBootstrap(
	data []byte,
	sourceServerID, advertiseServerID, bootstrapChannelID int,
) ([]byte, bool, error) {
	return build90CNOnlineScript(data, sourceServerID, advertiseServerID, bootstrapChannelID)
}

func build90CNOnlineScript(
	data []byte,
	sourceServerID, advertiseServerID, bootstrapChannelID int,
) ([]byte, bool, error) {
	if sourceServerID <= 0 || advertiseServerID < 0 || bootstrapChannelID <= 0 {
		return nil, false, errors.New("channel script source, advertisement, and bootstrap IDs are invalid")
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	var prefix strings.Builder
	serverSectionSeen := false
	inServer := false
	activeServer := 1
	records := make([][]string, 0, 64)
	bootstrapFound := false

	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if serverID, ok, err := parseServerSectionHeader(trimmed); ok {
			if err != nil {
				return nil, false, fmt.Errorf("channel script line %d: %w", lineNumber, err)
			}
			serverSectionSeen = true
			inServer = true
			activeServer = serverID
			continue
		}
		switch strings.ToLower(trimmed) {
		case "[/server]":
			inServer = false
			continue
		}
		if !serverSectionSeen {
			prefix.WriteString(line)
			prefix.WriteByte('\n')
			continue
		}
		if !inServer {
			continue
		}

		if trimmed == "" {
			continue
		}
		fields := splitFields(trimmed)
		if len(fields) == 0 {
			continue
		}
		if len(fields) == 1 {
			if id, err := strconv.Atoi(fields[0]); err == nil {
				activeServer = id
			}
			continue
		}

		start := 0
		if len(fields) >= 5 && isInt(fields[0]) && isChannelStart(fields, 1) {
			activeServer, _ = strconv.Atoi(fields[0])
			start = 1
		}
		for start < len(fields) {
			if !isChannelStart(fields, start) {
				return nil, false, fmt.Errorf("channel script line %d: invalid packed channel row", lineNumber)
			}
			end := len(fields)
			for next := start + 4; next < len(fields); next++ {
				if isChannelStart(fields, next) {
					end = next
					break
				}
			}
			if activeServer == sourceServerID {
				record := append([]string(nil), fields[start:end]...)
				id, _ := strconv.Atoi(record[0])
				channelType, _ := strconv.Atoi(record[2])
				record[2] = strconv.Itoa(OnlineTypeFor90CN(id, channelType))
				if id == bootstrapChannelID {
					bootstrapFound = true
				}
				for len(record) > 0 && record[len(record)-1] == "``" {
					record = record[:len(record)-1]
				}
				record = append(record, "``")
				records = append(records, record)
			}
			start = end
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, false, err
	}
	if len(records) == 0 {
		return nil, false, fmt.Errorf("channel script source server %d has no channels", sourceServerID)
	}

	var output strings.Builder
	output.WriteString(prefix.String())
	output.WriteString("[server]\n")
	output.WriteString(strconv.Itoa(advertiseServerID))
	output.WriteByte('\n')
	for _, record := range records {
		output.WriteString(strings.Join(record, " "))
		output.WriteByte('\n')
	}
	output.WriteString("[/server]\n")
	return []byte(output.String()), bootstrapFound, nil
}

// OnlineTypeFor90CN converts a raw PVF channel type into the type stored in
// the downloaded online directory. It must never be used for game CHANNELINFO.
func OnlineTypeFor90CN(channelID, channelType int) int {
	if channelType == 0 || channelType == 1 {
		return 22
	}
	if channelID == 1 && channelType == 2 {
		return 11
	}
	return channelType
}
