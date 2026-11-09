// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"strings"

	"github.com/jchv/go-webview2/webviewloader"
	"golang.org/x/sys/windows/registry"
)

// webview2ClientKey is the registry key of the Edge WebView2 Evergreen
// runtime client.
//
// The runtime registers itself under the EdgeUpdate client key. The key
// exists once the runtime is installed and its "pv" value carries the
// installed version.
const webview2ClientKey = `{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`

// webview2RegistryPaths are the registry paths the client key is looked up
// under.
//
// The runtime registers itself under HKLM and under HKCU, and a 32 bit
// installation registers itself under the WOW6432Node branch, so all of
// them are checked.
var webview2RegistryPaths = []string{
	`SOFTWARE\Microsoft\EdgeUpdate\Clients\` + webview2ClientKey,
	`SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\` + webview2ClientKey,
}

// webview2Available reports whether the WebView2 Evergreen runtime is
// installed, and which version is installed.
//
// The check is the documented registry check of the EdgeUpdate client key:
// the key exists once the runtime is installed and its "pv" value carries
// the installed version. The key is looked up under HKLM and HKCU, and
// under the WOW6432Node branch of both, because a 32 bit installation
// registers itself there.
//
// The registry check is complemented by the check of the loader of the
// runtime itself, so a machine with an unusual registry is not put into the
// degraded mode while the runtime is in fact installed.
func webview2Available() (bool, string) {
	if ok, version := webview2InstalledInRegistry(); ok {
		return true, version
	}
	if ok, version := webview2InstalledByLoader(); ok {
		return true, version
	}
	return false, ""
}

// webview2InstalledInRegistry reports the runtime through the registry.
func webview2InstalledInRegistry() (bool, string) {
	for _, root := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
		for _, path := range webview2RegistryPaths {
			key, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
			if err != nil {
				// The key does not exist: the runtime is not registered
				// under this root and this branch.
				continue
			}
			version, _, valueErr := key.GetStringValue("pv")
			_ = key.Close()
			if valueErr != nil {
				continue
			}
			if strings.TrimSpace(version) != "" {
				return true, version
			}
		}
	}
	return false, ""
}

// webview2InstalledByLoader reports the runtime through the loader of the
// WebView2 runtime.
//
// The loader asks the runtime itself for its version, so it answers for
// every installation the runtime supports, also the ones that are not
// registered under the EdgeUpdate client key.
func webview2InstalledByLoader() (bool, string) {
	version, err := webviewloader.GetInstalledVersion()
	if err != nil {
		return false, ""
	}
	version = strings.TrimSpace(version)
	return version != "", version
}
