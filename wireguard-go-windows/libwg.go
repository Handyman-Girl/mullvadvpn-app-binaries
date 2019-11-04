/* SPDX-License-Identifier: Apache-2.0
 *
 * Copyright (C) 2017-2019 Jason A. Donenfeld <Jason@zx2c4.com>. All Rights Reserved.
 * Copyright (C) 2019 Amagicom AB. All Rights Reserved.
 */

package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"bufio"
	"errors"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net"
	"os"
	"strings"

	"golang.org/x/sys/windows"

	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/ipc"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

type TunnelContext struct {
	device *device.Device
	uapi   net.Listener
	logger *device.Logger
}

var tunnels map[int32]TunnelContext

func init() {
	device.RoamingDisabled = true
	tunnels = make(map[int32]TunnelContext)
}

// Adjust logger to use the passed file descriptor for all output if the filedescriptor is valid
func newLogger(loggingFd int, level int) *device.Logger {
	logger := new(device.Logger)
	outputFile := os.NewFile(uintptr(loggingFd), "")
	var output io.Writer
	if outputFile != nil {
		output = outputFile
	} else {
		output = os.Stdout
	}

	logErr, logInfo, logDebug := func() (io.Writer, io.Writer, io.Writer) {
		if level >= device.LogLevelDebug {
			return output, output, output
		}
		if level >= device.LogLevelInfo {
			return output, output, ioutil.Discard
		}
		if level >= device.LogLevelError {
			return output, ioutil.Discard, ioutil.Discard
		}
		return ioutil.Discard, ioutil.Discard, ioutil.Discard
	}()

	logger.Debug = log.New(logDebug,
		"DEBUG: ",
		log.Ldate|log.Ltime,
	)
	logger.Info = log.New(logInfo,
		"INFO: ",
		log.Ldate|log.Ltime,
	)
	logger.Error = log.New(logErr,
		"ERROR: ",
		log.Ldate|log.Ltime,
	)

	return logger
}

// Find next free context slot
func getContextHandle() (int32, error) {
	var i int32
	for i = 0; i < math.MaxInt32; i++ {
		if _, exists := tunnels[i]; !exists {
			break
		}
	}

	if i == math.MaxInt32 {
		return 0, errors.New("Handle table is full")
	}

	return i, nil
}

//export wgTurnOn
func wgTurnOn(cIfaceName *C.char, mtu int, cSettings *C.char, loggingFd int, level int) int32 {
	logger := newLogger(loggingFd, level)

	if cIfaceName == nil {
		logger.Error.Println("cIfaceName is null")
		return -1
	}

	if cSettings == nil {
		logger.Error.Println("cSettings is null")
		return -1
	}

	contextHandle, err := getContextHandle()
	if err != nil {
		logger.Error.Println(err)
		return -1
	}

	settings := C.GoString(cSettings)
	ifaceName := C.GoString(cIfaceName)

	// {AFE43773-E1F8-4EBB-8536-576AB86AFE9A}
	networkId := windows.GUID { 0xafe43773, 0xe1f8, 0x4ebb, [8]byte{ 0x85, 0x36, 0x57, 0x6a, 0xb8, 0x6a, 0xfe, 0x9a } }

	watcher, err := watchInterfaces()
	if err != nil {
		logger.Error.Println(err)
		return -1
	}
	defer watcher.destroy()

	wintun, err := tun.CreateTUNWithRequestedGUID(ifaceName, &networkId, mtu)
	if err != nil {
		logger.Error.Println("Failed to create tunnel")
		logger.Error.Println(err)
		return -1
	}

	nativeTun := wintun.(*tun.NativeTun)

	actualInterfaceName, err := nativeTun.Name()
	if err != nil {
		nativeTun.Close()
		logger.Error.Println("Failed to determine name of wintun adapter")
		return -1
	}

	if actualInterfaceName != ifaceName {
		// WireGuard picked a different name for the adapter than the one we expected.
		// This indicates there is already an adapter with the name we intended to use.
		nativeTun.Close()
		logger.Error.Println("Failed to create adapter with specific name")
		return -1
	}

	device := device.NewDevice(wintun, logger)

	uapi, err := ipc.UAPIListen(ifaceName)
	if err != nil {
		logger.Error.Println("Failed to start UAPI")
		logger.Error.Println(err)
		device.Close()
		return -1
	}

	setError := device.IpcSetOperation(bufio.NewReader(strings.NewReader(settings)))
	if setError != nil {
		logger.Error.Println("Failed to set device configuration")
		logger.Error.Println(setError)
		uapi.Close()
		device.Close()
		return -1
	}

	device.Up()

	interfaces := []interfaceWatcherEvent{
		{
			luid: winipcfg.LUID(nativeTun.LUID()),
			family: windows.AF_INET,
		},
		{
			luid: winipcfg.LUID(nativeTun.LUID()),
			family: windows.AF_INET6,
		},
	}

	logger.Debug.Println("Waiting for interfaces to attach")

	if !watcher.join(interfaces, 5) {
		logger.Error.Println("Failed to wait for IP interfaces to become available")
		uapi.Close()
		device.Close()
		return -1
	}

	logger.Debug.Println("Interfaces OK")

	// Service UAPI.
	go func() {
		for {
			conn, err := uapi.Accept()
			if err != nil {
				logger.Info.Println("UAPI Accept() failed")
				logger.Info.Println(err)
				continue
			}
			go device.IpcHandle(conn)
		}
	}()

	tunnels[contextHandle] = TunnelContext{
		device: device,
		uapi: uapi,
		logger: logger,
	}

	return contextHandle
}

//export wgTurnOff
func wgTurnOff(contextHandle int32) {
	context, ok := tunnels[contextHandle]
	if !ok {
		return
	}
	delete(tunnels, contextHandle)
	context.uapi.Close()
	context.device.Close()
}

//export wgRebindTunnelSocket
func wgRebindTunnelSocket(family uint16, interfaceIndex uint32) {
	for _, tunnel := range tunnels {
		blackhole := (interfaceIndex == 0)
		if family == windows.AF_INET {
			tunnel.logger.Info.Printf("Binding v4 socket to interface %d (blackhole=%v)", interfaceIndex, blackhole)
			err := tunnel.device.BindSocketToInterface4(interfaceIndex, blackhole)
			if err != nil {
				tunnel.logger.Info.Println(err)
			}
		} else if family == windows.AF_INET6 {
			tunnel.logger.Info.Printf("Binding v6 socket to interface %d (blackhole=%v)", interfaceIndex, blackhole)
			err := tunnel.device.BindSocketToInterface6(interfaceIndex, blackhole)
			if err != nil {
				tunnel.logger.Info.Println(err)
			}
		}
	}
}

//export wgVersion
func wgVersion() *C.char {
	return C.CString(device.WireGuardGoVersion)
}

func main() {}
