// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import "github.com/bashrusakh/hfdesk/internal/logging"

// downloadLog is the logger of the download subsystem.
//
// It resolves the current default logger on every call, so a record emitted
// after the application installed its logging pipeline still reaches that
// pipeline. The downloader never installs a pipeline itself: the shared
// startup layer owns that. Without a pipeline the records go to the standard
// library default logger, exactly like the log.Printf calls they replaced.
//
// Only facts the downloader already knows are logged, and never a
// credential: the Hugging Face token, the proxy credentials and the
// Authorization header never reach a record. See the credential audit in
// proxy.go for the proxy side of that rule.
var downloadLog = logging.NewComponentLogger("download")
