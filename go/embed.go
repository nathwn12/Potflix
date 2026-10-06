package main

import _ "embed"

// iconICO carries the tray icon in the binary. The Java app embedded
// src\main\resources\icon.png; this port embeds assets\icon.ico (a single
// 256x256 BMP/DIB frame), so no icon file needs to ship beside the exe.
//
//go:embed assets/icon.ico
var iconICO []byte
