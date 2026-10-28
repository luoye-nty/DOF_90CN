// Package carrygold owns the gold carry-cap rule: the PVF base limit a
// character's level grants, the durable upgrade stage the player has bought on
// top of it, and the credit helper every gold faucet uses so the cap actually
// bites when gold is gained instead of only being displayed.
//
// The base limit is real PVF data, not a constant: `etc/(r)goldlimitbylevel.etc`
// carries `[gold limit from level]` as level/limit pairs, rising from 0 at
// level 0 to 400,000,000 from level 60 on. The upgrade adds one 亿 per stage on
// top of that, which is why the client's confirm dialog offers 4亿 -> 5亿 for a
// level-capped character.
//
// The stage itself is decoded from the current client, which is authoritative
// for this feature because the release-side server had no handler for it at
// all — the op978 request was received, dropped, and the confirm button
// therefore did nothing.
//
// Request, legacy cmd1/type978 built by sub_1AEBF10:
//
//	mov al, [0x052B5C04]   ; the client's own cached stage
//	inc al
//	push 978
//	...                    ; put_header
//	mov [ebp-7], al        ; the only field the builder writes
//	push 0x0E; push [ebp-0x14]; put_binary
//
// The blob is 14 bytes copied straight off the stack and only its **last**
// byte is initialized, so bytes 0..12 are uninitialized stack residue and must
// be ignored.
//
// Response, class1/op978 read by sub_1D23D60: the ordinary success envelope
// byte must be non-zero, then u8 stage and u32 result. A non-zero result takes
// the client's failure branch; zero stores the stage into 0x052B5C04 and
// formats client string 0x1194D with stage+4.
//
// Bounds are the client's own: sub_1AEBFF0 and three other sites refuse to
// offer an upgrade once the stage reaches 4, and the stage is displayed as
// stage+4, so the five stages read 4..8 (亿) in game.
package carrygold

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	// StageStat is the durable character stat holding the purchased stage.
	StageStat = "carry_gold_stage"

	// MaxStage is the last stage the client will ever offer.
	MaxStage = byte(4)

	// StageCost is the flat price the client promises the player. Its confirm
	// dialog reads "扩充时将消耗500万金币" while raising the cap by one 亿, and
	// that number is baked into the client string: it is absent from the
	// request, from the runtime Script.pvf, and from every code immediate in
	// DNF.exe, so nothing per-stage can be derived and the charge is flat.
	StageCost = int64(5_000_000)

	// Unit is one 亿, the granularity one purchased stage adds.
	Unit = int64(100_000_000)

	// DisplayedBase is the cap the client's own string formats at stage 0. It
	// is the level-capped case, which is the only one the dialog can show.
	DisplayedBase = 4

	// LevelLimitPath is the PVF table holding the per-level base cap.
	LevelLimitPath = "etc/(r)goldlimitbylevel.etc"

	// LevelLimitKey is the section inside that table.
	LevelLimitKey = "gold limit from level"
)

var (
	ErrSourceRequired  = errors.New("carry gold pvf source is required")
	ErrLevelLimitTable = errors.New("carry gold level limit table is malformed")
)

// Source is the PVF text reader the loader needs.
type Source interface {
	ReadText(relativePath string) (string, error)
}

type levelLimitEntry struct {
	level int
	limit int64
}

// LevelLimits is the parsed `[gold limit from level]` table. It is immutable
// once loaded.
type LevelLimits struct {
	entries []levelLimitEntry
}

// LoadLevelLimits parses the real PVF table. It never falls back to a guessed
// table: a missing or malformed file is an error the caller has to surface.
func LoadLevelLimits(ctx context.Context, source Source) (*LevelLimits, error) {
	if source == nil {
		return nil, ErrSourceRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	text, err := source.ReadText(LevelLimitPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", LevelLimitPath, err)
	}
	document, err := dnfpvf.Parse(LevelLimitPath, text)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", LevelLimitPath, err)
	}
	return parseLevelLimits(document.Ints(LevelLimitKey))
}

func parseLevelLimits(values []int64) (*LevelLimits, error) {
	if len(values) < 2 || len(values)%2 != 0 {
		return nil, fmt.Errorf("%w: %s has %d values", ErrLevelLimitTable, LevelLimitKey, len(values))
	}
	entries := make([]levelLimitEntry, 0, len(values)/2)
	for index := 0; index+1 < len(values); index += 2 {
		level := values[index]
		limit := values[index+1]
		if level < 0 || limit < 0 {
			return nil, fmt.Errorf("%w: level=%d limit=%d", ErrLevelLimitTable, level, limit)
		}
		entries = append(entries, levelLimitEntry{level: int(level), limit: limit})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].level < entries[j].level })
	return &LevelLimits{entries: entries}, nil
}

// Limit is the base cap a character of this level may hold, before any bought
// stage. Levels between two rows keep the lower row's value, and a level above
// the last row keeps the last row's value, which is how the client's own table
// lookup behaves.
func (l *LevelLimits) Limit(level int) int64 {
	if l == nil || len(l.entries) == 0 {
		return 0
	}
	if level < l.entries[0].level {
		return l.entries[0].limit
	}
	index := sort.Search(len(l.entries), func(i int) bool { return l.entries[i].level > level })
	return l.entries[index-1].limit
}

// Len reports how many level rows were parsed. It exists so the preload can
// log what it installed.
func (l *LevelLimits) Len() int {
	if l == nil {
		return 0
	}
	return len(l.entries)
}

// installed holds the process-wide table. Static game data is read by every
// gold faucet in every module, so it is installed once at startup instead of
// being threaded through a dozen commands. A nil table means "not loaded", and
// then no cap is applied at all — the rule refuses to invent a limit the PVF
// did not supply.
var installed atomic.Pointer[LevelLimits]

// Install publishes the table for every faucet. The dnfbridge PVF preload owns
// the single call; passing nil clears it, which only tests do.
func Install(limits *LevelLimits) {
	installed.Store(limits)
}

// Installed returns the published table, or nil when none has been loaded.
func Installed() *LevelLimits {
	return installed.Load()
}

// Stage reads the durable stage out of a character stat map, clamped to what
// the client accepts. A missing or non-positive stat is stage zero, which is
// the un-upgraded default.
func Stage(stats map[string]int64) byte {
	if stats == nil {
		return 0
	}
	return StageFromValue(stats[StageStat])
}

// StageFromValue clamps a raw persisted value into the client's stage range.
func StageFromValue(value int64) byte {
	if value <= 0 {
		return 0
	}
	if value >= int64(MaxStage) {
		return MaxStage
	}
	return byte(value)
}

// DisplayedLimit reproduces the client's own string, which formats stage+4 (亿)
// regardless of level. It is a log/UI value, never the rule.
func DisplayedLimit(stage byte) int {
	return DisplayedBase + int(StageFromValue(int64(stage)))
}

// StageBonus is what one purchased stage adds on top of the PVF base limit.
func StageBonus(stage byte) int64 {
	return int64(StageFromValue(int64(stage))) * Unit
}

// LimitForCharacter is the character's effective carry cap: the PVF base limit
// for its level plus the bought stages. It is zero when no PVF table has been
// installed, which every helper here reads as "uncapped".
func LimitForCharacter(character dnfrepo.CharacterRecord) int64 {
	table := Installed()
	if table == nil {
		return 0
	}
	base := table.Limit(character.Level)
	if base <= 0 {
		return 0
	}
	return base + StageBonus(Stage(character.Stats))
}

// CreditBalance applies amount to balance without ever exceeding limit, and
// reports how much had to be discarded. A limit of zero means uncapped.
//
// It deliberately never *reduces* an existing balance. Release-side gold was
// never capped, so a character can already hold more than its level and stage
// allow; clamping such a balance downwards would delete gold the player owns.
// A balance already at or above the limit simply gains nothing.
func CreditBalance(balance int64, amount int64, limit int64) (after int64, discarded int64) {
	if amount <= 0 || limit <= 0 {
		return balance + amount, 0
	}
	if balance >= limit {
		return balance, amount
	}
	room := limit - balance
	if amount <= room {
		return balance + amount, 0
	}
	return limit, amount - room
}

// Credit is CreditBalance against the character's own level and stage.
func Credit(character dnfrepo.CharacterRecord, amount int64) (after int64, discarded int64) {
	var balance int64
	if character.Stats != nil {
		balance = character.Stats["gold"]
	}
	return CreditBalance(balance, amount, LimitForCharacter(character))
}

// Room is how much more gold the character may still gain. It is zero once the
// balance has reached the cap, and math.MaxInt64 when no cap is installed.
func Room(character dnfrepo.CharacterRecord) int64 {
	limit := LimitForCharacter(character)
	if limit <= 0 {
		return int64(^uint64(0) >> 1)
	}
	var balance int64
	if character.Stats != nil {
		balance = character.Stats["gold"]
	}
	if balance >= limit {
		return 0
	}
	return limit - balance
}
