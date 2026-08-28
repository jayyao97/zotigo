package workspace

import (
	"strings"
	"unicode"
)

func storageSlug(name string, fallback string) string {
	var slug strings.Builder
	separator := false
	for _, value := range strings.TrimSpace(name) {
		if unicode.IsLetter(value) || unicode.IsNumber(value) {
			if separator && slug.Len() > 0 {
				slug.WriteByte('-')
			}
			slug.WriteRune(unicode.ToLower(value))
			separator = false
			continue
		}
		separator = slug.Len() > 0
	}
	if slug.Len() == 0 {
		return fallback
	}
	return slug.String()
}

func storageName(name string, id string, idPrefix string, fallback string) string {
	stableID := strings.TrimPrefix(id, idPrefix)
	if len(stableID) > 8 {
		stableID = stableID[:8]
	}
	return storageSlug(name, fallback) + "-" + stableID
}

func projectStorageName(name string, id string) string {
	return storageName(name, id, "project_", "project")
}

func workspaceStorageName(title string, id string) string {
	return storageName(title, id, "workspace_", "workspace")
}
