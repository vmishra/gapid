// Copyright (C) 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/gapid/core/app"
	"github.com/google/gapid/core/app/crash"
	"github.com/google/gapid/core/event/task"
	"github.com/google/gapid/core/log"
	"github.com/google/gapid/core/os/device"
	"github.com/google/gapid/gapis/service"
	"github.com/google/gapid/gapis/service/path"
)

type traceVerb struct{ TraceFlags }

func init() {
	verb := &traceVerb{}
	verb.TraceFlags.Disable.PCS = true

	app.AddVerb(&app.Verb{
		Name:      "trace",
		ShortHelp: "Captures a gfx trace from an application",
		Action:    verb,
	})
}

func (verb *traceVerb) Run(ctx context.Context, flags flag.FlagSet) error {
	client, err := getGapis(ctx, verb.Gapis, GapirFlags{})
	if err != nil {
		return log.Err(ctx, err, "Failed to connect to the GAPIS server")
	}
	defer client.Close()

	traceURI := verb.URI

	if traceURI == "" && verb.Local.Port == 0 {
		if flags.NArg() != 1 {
			app.Usage(ctx, "Expected application name")
			return nil
		}
		traceURI = flags.Arg(0)
	}

	var traceDevice *path.Device
	out := "capture.gfxtrace"
	var port uint32
	var uri string

	if verb.Local.Port != 0 {
		serverInfo, err := client.GetServerInfo(ctx)
		if err != nil {
			return err
		}
		traceDevice = serverInfo.GetServerLocalDevice()
		if traceDevice.GetID() == nil {
			return fmt.Errorf("The server was not started with a local device for tracing")
		}
		port = uint32(verb.Local.Port)
	} else {
		type info struct {
			uri        string
			device     *path.Device
			deviceName string
			name       string
		}
		var found []info

		// Find the actual trace URI from all of the devices

		devices, err := filterDevices(ctx, &verb.DeviceFlags, client)
		if err != nil {
			return err
		}

		if len(devices) == 0 {
			return fmt.Errorf("Could not find matching device")
		}

		for _, dev := range devices {
			targets, err := client.FindTraceTargets(ctx, &service.FindTraceTargetsRequest{
				Device: dev,
				Uri:    traceURI,
			})
			if err != nil {
				continue
			}

			dd, err := client.Get(ctx, dev.Path(), nil)
			if err != nil {
				return err
			}
			d := dd.(*device.Instance)

			for _, target := range targets {
				name := target.Name
				switch {
				case target.FriendlyApplication != "":
					name = target.FriendlyApplication
				case target.FriendlyExecutable != "":
					name = target.FriendlyExecutable
				}

				found = append(found, info{
					uri:        target.Uri,
					deviceName: d.Name,
					device:     dev,
					name:       name,
				})
			}
		}

		if len(found) == 0 {
			return fmt.Errorf("Could not find %+v to trace on any device", traceURI)
		}

		if len(found) > 1 {
			sb := strings.Builder{}
			fmt.Fprintf(&sb, "Found %v candidates: \n", traceURI)
			for i, f := range found {
				if i == 0 || found[i-1].deviceName != f.deviceName {
					fmt.Fprintf(&sb, "  %v:\n", f.deviceName)
				}
				fmt.Fprintf(&sb, "    %v\n", f.uri)
			}
			return log.Errf(ctx, nil, "%v", sb.String())
		}

		fmt.Printf("Tracing %+v", found[0].uri)
		out = found[0].name + ".gfxtrace"
		uri = found[0].uri
	}

	if verb.Out != "" {
		out = verb.Out
	}

	options := &service.TraceOptions{
		Device: traceDevice,
		Apis:   []string{},
		AdditionalCommandLineArgs: verb.AdditionalArgs,
		Cwd:                   verb.WorkingDir,
		Environment:           verb.Env,
		Duration:              float32(verb.For.Seconds()),
		ObserveFrameFrequency: uint32(verb.Observe.Frames),
		ObserveDrawFrequency:  uint32(verb.Observe.Draws),
		StartFrame:            uint32(verb.Start.At.Frame),
		FramesToCapture:       uint32(verb.Capture.Frames),
		DisablePcs:            verb.Disable.PCS,
		RecordErrorState:      verb.Record.Errors,
		DeferStart:            verb.Start.Defer,
		NoBuffer:              verb.No.Buffer,
		HideUnknownExtensions: verb.Disable.Unknown.Extensions,
		ClearCache:            verb.Clear.Cache,
		ServerLocalSavePath:   out,
	}

	if uri != "" {
		options.App = &service.TraceOptions_Uri{
			uri,
		}
	} else {
		options.App = &service.TraceOptions_Port{
			port,
		}
	}

	switch verb.API {
	case "vulkan":
		options.Apis = []string{"Vulkan"}
	case "gles":
		// TODO: Separate these two out once we can trace Vulkan with OpenGL ES.
		options.Apis = []string{"OpenGLES", "GVR"}
	case "":
		options.Apis = []string{"Vulkan", "OpenGLES", "GVR"}
	default:
		return fmt.Errorf("Unknown API %s", verb.API)
	}

	handler, err := client.Trace(ctx)
	if err != nil {
		return err
	}
	defer handler.Dispose()

	defer app.AddInterruptHandler(func() {
		handler.Dispose()
	})()

	status, err := handler.Initialize(options)
	if err != nil {
		return err
	}
	log.I(ctx, "Trace Status %+v", status)

	handlerInstalled := false

	return task.Retry(ctx, 0, time.Second*3, func(ctx context.Context) (retry bool, err error) {
		status, err = handler.Event(service.TraceEvent_Status)
		if err == io.EOF {
			return true, nil
		}
		if err != nil {
			log.I(ctx, "Error %+v", err)
			return true, err
		}
		if status == nil {
			return true, nil
		}

		if status.BytesCaptured > 0 {
			if !handlerInstalled {
				crash.Go(func() {
					reader := bufio.NewReader(os.Stdin)
					if options.DeferStart {
						println("Press enter to start capturing...")
						_, _ = reader.ReadString('\n')
						_, _ = handler.Event(service.TraceEvent_Begin)
					}
					println("Press enter to stop capturing...")
					_, _ = reader.ReadString('\n')
					handler.Event(service.TraceEvent_Stop)
				})
				handlerInstalled = true
			}
			log.I(ctx, "Capturing %+v", status.BytesCaptured)
		}
		if status.Status == service.TraceStatus_Done {
			return true, nil
		}
		return false, nil
	})
}
