package steamcontroller_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Alia5/VIIPER/device/steamcontroller"
	"github.com/Alia5/VIIPER/usbip"
)

const runtimeStateRaceIterations = 10000

func runConcurrent(t *testing.T, operations ...func()) {
	t.Helper()
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(len(operations))
	for _, operation := range operations {
		go func(operation func()) {
			defer wg.Done()
			<-start
			operation()
		}(operation)
	}
	close(start)
	wg.Wait()
}

func newRuntimeRaceController(t *testing.T) *steamcontroller.SteamController {
	t.Helper()
	controller, err := steamcontroller.New(nil)
	if err != nil {
		t.Fatalf("create Steam Controller: %v", err)
	}
	controller.UpdateInputState(&steamcontroller.InputState{
		AccelX: 111,
		AccelY: -222,
		AccelZ: 333,
		GyroX:  444,
		GyroY:  -555,
		GyroZ:  666,
	})
	return controller
}

func sendFeatureCommand(controller *steamcontroller.SteamController, command byte, payload ...byte) bool {
	data := make([]byte, 2+len(payload))
	data[0] = 0
	data[1] = command
	copy(data[2:], payload)
	_, ok := controller.HandleControl(
		0x21,
		0x09,
		0x0300,
		steamControllerInterface,
		uint16(len(data)),
		data,
	)
	return ok
}

func setRuntimeSetting(controller *steamcontroller.SteamController, setting byte, value uint16) bool {
	return sendFeatureCommand(controller, steamcontroller.FeatureSetSettingsValues,
		3, setting, byte(value), byte(value>>8))
}

func readFeature(controller *steamcontroller.SteamController, command byte) ([]byte, bool) {
	return controller.HandleControl(
		0xa1,
		0x01,
		0x0300|uint16(command),
		steamControllerInterface,
		steamcontroller.InputReportLen,
		nil,
	)
}

func TestConcurrentSettingsMutationAndControllerPolling(t *testing.T) {
	controller := newRuntimeRaceController(t)
	var invalid atomic.Bool

	runConcurrent(t,
		func() {
			for i := 0; i < runtimeStateRaceIterations; i++ {
				report := controller.HandleTransfer(context.Background(), steamControllerEndpoint, usbip.DirIn, nil)
				if len(report) != steamcontroller.InputReportLen {
					invalid.Store(true)
				}
			}
		},
		func() {
			for i := 0; i < runtimeStateRaceIterations; i++ {
				value := uint16(steamcontroller.GyroModeSendRawAccel | steamcontroller.GyroModeSendRawGyro)
				if i%2 == 0 {
					value = steamcontroller.GyroModeOff
				}
				if !setRuntimeSetting(controller, steamcontroller.SettingIMUMode, value) {
					invalid.Store(true)
				}
			}
		},
	)

	if invalid.Load() {
		t.Fatal("concurrent settings mutation produced an invalid controller operation")
	}
	if report := controller.HandleTransfer(context.Background(), steamControllerEndpoint, usbip.DirIn, nil); len(report) != steamcontroller.InputReportLen {
		t.Fatalf("final controller report length = %d, want %d", len(report), steamcontroller.InputReportLen)
	}
}

func TestConcurrentDigitalMappingMutationAndLizardKeyboardPolling(t *testing.T) {
	controller := newRuntimeRaceController(t)
	var invalid atomic.Bool

	runConcurrent(t,
		func() {
			for i := 0; i < runtimeStateRaceIterations; i++ {
				report := controller.HandleTransfer(context.Background(), steamControllerKeyboardEndpoint, usbip.DirIn, nil)
				if len(report) != 8 {
					invalid.Store(true)
				}
			}
		},
		func() {
			commands := []byte{
				steamcontroller.FeatureClearDigitalMappings,
				steamcontroller.FeatureSetDefaultMappings,
				steamcontroller.FeatureSetDigitalMappings,
			}
			for i := 0; i < runtimeStateRaceIterations; i++ {
				if !sendFeatureCommand(controller, commands[i%len(commands)]) {
					invalid.Store(true)
				}
			}
		},
	)

	if invalid.Load() {
		t.Fatal("concurrent digital mapping mutation produced an invalid keyboard operation")
	}
}

func TestConcurrentControllerResetAndReportGeneration(t *testing.T) {
	controller := newRuntimeRaceController(t)
	var invalid atomic.Bool

	runConcurrent(t,
		func() {
			for i := 0; i < runtimeStateRaceIterations; i++ {
				report := controller.HandleTransfer(context.Background(), steamControllerEndpoint, usbip.DirIn, nil)
				if len(report) != steamcontroller.InputReportLen {
					invalid.Store(true)
				}
			}
		},
		func() {
			commands := []byte{
				steamcontroller.FeatureLoadDefaultSettings,
				steamcontroller.FeatureFactoryReset,
				steamcontroller.FeatureClearSettingsValues,
			}
			for i := 0; i < runtimeStateRaceIterations; i++ {
				if !sendFeatureCommand(controller, commands[i%len(commands)]) {
					invalid.Store(true)
				}
			}
		},
	)

	if invalid.Load() {
		t.Fatal("concurrent controller reset produced an invalid report")
	}
}

func TestConcurrentIMUResetInputUpdatesAndControllerPolling(t *testing.T) {
	controller := newRuntimeRaceController(t)
	var invalid atomic.Bool

	runConcurrent(t,
		func() {
			for i := 0; i < runtimeStateRaceIterations; i++ {
				report := controller.HandleTransfer(context.Background(), steamControllerEndpoint, usbip.DirIn, nil)
				if len(report) != steamcontroller.InputReportLen {
					invalid.Store(true)
				}
			}
		},
		func() {
			for i := 0; i < runtimeStateRaceIterations; i++ {
				controller.UpdateInputState(&steamcontroller.InputState{
					AccelX: int16(i),
					GyroX:  int16(-i),
				})
			}
		},
		func() {
			for i := 0; i < runtimeStateRaceIterations; i++ {
				if !sendFeatureCommand(controller, steamcontroller.FeatureResetIMU) {
					invalid.Store(true)
				}
			}
		},
	)

	if invalid.Load() {
		t.Fatal("concurrent IMU reset produced an invalid controller operation")
	}
}

func TestConcurrentFeatureReadsAndRuntimeStateMutations(t *testing.T) {
	controller := newRuntimeRaceController(t)
	var invalid atomic.Bool
	readCommands := []byte{
		steamcontroller.FeatureGetDeviceInfo,
		steamcontroller.FeatureGetAttributesValues,
		steamcontroller.FeatureGetStringAttribute,
		steamcontroller.FeatureGetDigitalMappings,
		steamcontroller.FeatureGetSettingsValues,
	}

	runConcurrent(t,
		func() {
			for i := 0; i < runtimeStateRaceIterations; i++ {
				response, ok := readFeature(controller, readCommands[i%len(readCommands)])
				if !ok || len(response) != steamcontroller.InputReportLen {
					invalid.Store(true)
				}
			}
		},
		func() {
			for i := 0; i < runtimeStateRaceIterations; i++ {
				if !setRuntimeSetting(controller, steamcontroller.SettingLizardMode, uint16(i%2)) {
					invalid.Store(true)
				}
			}
		},
	)

	if invalid.Load() {
		t.Fatal("concurrent feature reads produced an invalid response")
	}
}
