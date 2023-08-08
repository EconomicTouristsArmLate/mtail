//go:build !windows
// +build !windows

package daemon

// unused on non-windows platforms
var SVCStopChan = make(chan bool)
