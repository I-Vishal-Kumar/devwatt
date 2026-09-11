// Package icon holds the one devwatt icon, embedded, so the tray, the
// dashboard window and the Start menu shortcut all show the same picture
// and nothing has to find a file at run time.
package icon

import _ "embed"

// Bytes is devwatt.ico: 16, 32 and 48 px classic DIB entries.
//
//go:embed devwatt.ico
var Bytes []byte
