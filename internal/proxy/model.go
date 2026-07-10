package proxy

import "strings"

var publicModels = []string{"grok-4.5", "composer-2.5-fast"}

func canonicalModel(model string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(model))
	if index := strings.LastIndexByte(key, '/'); index >= 0 {
		key = key[index+1:]
	}
	for {
		next := strings.TrimSuffix(strings.TrimSuffix(key, "-cursor"), "-codex")
		if next == key {
			break
		}
		key = next
	}
	switch key {
	case "grok-4.5", "grok-build":
		return "grok-4.5", true
	case "composer-2.5", "grok-composer-2.5", "composer-2.5-fast", "grok-composer-2.5-fast":
		return "grok-composer-2.5-fast", true
	default:
		return "", false
	}
}
