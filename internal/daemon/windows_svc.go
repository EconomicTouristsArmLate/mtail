//go:build windows
// +build windows

// Adapted from prometheus/windows_exporter, MIT licensed:
//
// The MIT License (MIT)
//
// Copyright (c) 2016 Martin Lindhe
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package daemon

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

const (
	serviceName = "mtail"
)

type mtailService struct {
	stopCh chan<- bool
}

var logger *eventlog.Log

func (s *mtailService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}
	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}
loop:
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				_ = logger.Info(100, "Service Stop Received")
				s.stopCh <- true
				break loop
			default:
				_ = logger.Error(102, fmt.Sprintf("unexpected control request #%d", c))
			}
		}
	}
	changes <- svc.Status{State: svc.StopPending}
	return
}

var SVCStopChan = make(chan bool)

func init() {
	isService, err := svc.IsWindowsService()
	if err != nil {
		logger, err := eventlog.Open(serviceName)
		if err != nil {
			os.Exit(2)
		}
		_ = logger.Error(102, fmt.Sprintf("Failed to detect service: %v", err))
		os.Exit(1)
	}

	if isService {
		logger, err := eventlog.Open(serviceName)
		if err != nil {
			os.Exit(2)
		}
		_ = logger.Info(100, "Attempting to start exporter service")
		go func() {
			err = svc.Run(serviceName, &mtailService{stopCh: SVCStopChan})
			if err != nil {
				_ = logger.Error(102, fmt.Sprintf("Failed to start service: %v", err))
			}
		}()
	}
}
