//go:build windows

package autostart

import "golang.org/x/sys/windows/registry"

func openRunKey() (registry.Key, error) {
	return registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
}
