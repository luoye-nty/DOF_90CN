package dnfbridge

import (
	"fmt"
	"path"
	"strings"

	"longheng.io/server/internal/modules/dnf/actionobject"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

// passiveObjectActionScript resolves the action script a placed passive object
// runs: passiveobject.lst gives its `.obj`, the `.obj`'s `[basic action]` gives
// the `.act` next to it.
//
// These scripts are what drive `[DUNGEON SYMBOL CALC]` / `[GATE ENABLE]`, and
// nothing in this server ran them before: 能源储藏室's boss door is held shut by
// `storage gate open` and stayed shut forever because no one was counting.
//
// A missing or unparsable definition is not an error a room transition should
// die on, so it is cached as "no script" and reported by the bool.
func (c *pvfDungeonMonsterCatalog) passiveObjectActionScript(id int64) (actionobject.Script, bool, error) {
	if c == nil || c.source == nil {
		return actionobject.Script{}, false, errDungeonMonsterCatalogUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensurePassiveObjectIndexLocked()
	if c.passiveObjectErr != nil {
		return actionobject.Script{}, false, c.passiveObjectErr
	}
	if c.actionScriptLoaded[id] {
		script, found := c.actionScripts[id]
		return script, found, nil
	}
	if c.actionScriptLoaded == nil {
		c.actionScriptLoaded = make(map[int64]bool)
	}
	c.actionScriptLoaded[id] = true

	listedPath, listed := c.passiveObjectPaths[id]
	if !listed {
		return actionobject.Script{}, false, nil
	}
	objectPath, objectText, err := readDungeonPassiveObjectText(c.source, listedPath)
	if err != nil {
		if isDungeonPassiveObjectDefinitionMissing(err) {
			return actionobject.Script{}, false, nil
		}
		return actionobject.Script{}, false, fmt.Errorf("read passive object id=%d path=%s: %w", id, listedPath, err)
	}
	objectDocument, err := dnfpvf.Parse(objectPath, objectText)
	if err != nil {
		return actionobject.Script{}, false, fmt.Errorf("parse passive object id=%d path=%s: %w", id, objectPath, err)
	}
	basicAction, _ := objectDocument.Text("basic action")
	scriptPath := resolveDungeonActionObjectScriptPath(objectPath, basicAction)
	if scriptPath == "" {
		return actionobject.Script{}, false, nil
	}
	scriptText, err := c.source.ReadText(scriptPath)
	if err != nil {
		if isDungeonPassiveObjectDefinitionMissing(err) {
			return actionobject.Script{}, false, nil
		}
		return actionobject.Script{}, false, fmt.Errorf("read action script id=%d path=%s: %w", id, scriptPath, err)
	}
	scriptDocument, err := dnfpvf.Parse(scriptPath, scriptText)
	if err != nil {
		return actionobject.Script{}, false, fmt.Errorf("parse action script id=%d path=%s: %w", id, scriptPath, err)
	}
	script, err := actionobject.Parse(scriptDocument)
	if err != nil {
		return actionobject.Script{}, false, fmt.Errorf("decode action script id=%d path=%s: %w", id, scriptPath, err)
	}
	if !script.Drives() {
		// Animation-only scripts are the overwhelming majority. Dropping them
		// here keeps the cache and every room's instance list small.
		return actionobject.Script{}, false, nil
	}
	if c.actionScripts == nil {
		c.actionScripts = make(map[int64]actionobject.Script)
	}
	c.actionScripts[id] = script
	return script, true, nil
}

// resolveDungeonActionObjectScriptPath joins a `.obj`'s `[basic action]` to the
// directory that holds it: `Action/Gate_Control_Left.act` beside
// passiveobject/.../enegy_save/gate_control_left.obj.
func resolveDungeonActionObjectScriptPath(objectPath, basicAction string) string {
	action := strings.TrimPrefix(strings.TrimSpace(strings.ReplaceAll(basicAction, "\\", "/")), "/")
	if action == "" {
		return ""
	}
	directory := path.Dir(cleanDungeonPassiveObjectPath(objectPath))
	if directory == "." || directory == "" {
		return path.Clean(action)
	}
	if strings.HasPrefix(strings.ToLower(action), strings.ToLower(directory)+"/") {
		return path.Clean(action)
	}
	return path.Join(directory, action)
}
