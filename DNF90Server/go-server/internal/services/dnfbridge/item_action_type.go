package dnfbridge

import (
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

// PVF item action types.
//
// A stackable declares what using it does in an [action type] block:
//
//	[action type]
//	`[character name change]`
//	[/action type]
//
// The block is a section and its payload is an ordinary string token, so the
// markers come back from Document.Texts("action type") with the surrounding
// brackets still attached and one token possibly listing several markers.
//
// This is the contract used to recognize items whose behaviour the server has to
// implement. Before it existed the rename path matched two hard-coded PVF file
// names, so every other 角色更名卡 in the runtime PVF (event cards, localization
// -key cards, the account-bound 冒险家征集令 card, ...) was rejected as an invalid
// card even though the client-side item is identical.
const currentItemActionTypeCharacterNameChange = "character name change"

// currentItemDeclaresActionType reports whether the document declares the given
// action type. Comparison ignores case, bracket markers and repeated spaces, so
// `[Character Name Change]` and `character name change` both match.
func currentItemDeclaresActionType(document *dnfpvf.Document, actionType string) bool {
	want := currentItemActionTypeKey(actionType)
	if want == "" {
		return false
	}
	for _, value := range currentItemActionTypes(document) {
		if currentItemActionTypeKey(value) == want {
			return true
		}
	}
	return false
}

// currentItemActionTypes returns every marker declared by the document's
// [action type] block(s), with the brackets and backquotes already removed.
func currentItemActionTypes(document *dnfpvf.Document) []string {
	if document == nil {
		return nil
	}
	var out []string
	for _, value := range document.Texts("action type") {
		out = append(out, currentItemActionTypeMarkers(value)...)
	}
	return out
}

// currentItemActionTypeMarkers splits one [action type] value into markers.
// The value is usually a single `[marker]`, but a PVF may list several inside one
// token ("`[a]` `[b]`" arrives as two tokens, "[a] [b]" as one) and a few files
// write the marker bare, without brackets.
func currentItemActionTypeMarkers(value string) []string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	var out []string
	for offset := 0; offset < len(trimmed); {
		open := strings.IndexByte(trimmed[offset:], '[')
		if open < 0 {
			break
		}
		open += offset
		closeIndex := strings.IndexByte(trimmed[open:], ']')
		if closeIndex < 0 {
			break
		}
		marker := strings.TrimSpace(trimmed[open+1 : open+closeIndex])
		if marker != "" {
			out = append(out, marker)
		}
		offset = open + closeIndex + 1
	}
	if len(out) == 0 {
		out = append(out, trimmed)
	}
	return out
}

// currentItemActionTypeKey normalizes a marker for comparison.
func currentItemActionTypeKey(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "[]`")
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}
