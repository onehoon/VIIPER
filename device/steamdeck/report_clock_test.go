package steamdeck

import (
	"math"
	"testing"
	"time"
)

func TestControllerReportClockUsesStrictFutureAndSkipsLateSlots(t *testing.T) {
	origin := time.Unix(1, 0)
	for _, tc := range []struct {
		name string
		now  time.Duration
		want uint64
	}{
		{name: "at origin", now: 0, want: 1},
		{name: "one nanosecond before slot", now: 4*time.Millisecond - time.Nanosecond, want: 1},
		{name: "halfway to first slot", now: 2 * time.Millisecond, want: 1},
		{name: "exactly on slot", now: 4 * time.Millisecond, want: 2},
		{name: "between slots", now: 17 * time.Millisecond, want: 5},
		{name: "resume after 110ms absence", now: 110 * time.Millisecond, want: 28},
		{name: "resume after one second absence", now: time.Second, want: 251},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nextFutureControllerReportSlot(origin, origin.Add(tc.now), 0)
			if got != tc.want {
				t.Fatalf("next future slot = %d, want %d", got, tc.want)
			}
		})
	}

	if got, ok := latestElapsedControllerReportSlot(origin, origin.Add(17*time.Millisecond), 0); !ok || got != 4 {
		t.Fatalf("latest elapsed slot at 17ms = %d, %v; want slot 4 (16ms), true", got, ok)
	}
	if got, ok := latestElapsedControllerReportSlot(origin, origin.Add(17*time.Millisecond), 4); ok {
		t.Fatalf("already-served latest slot returned %d, want no eligible slot", got)
	}
	if got := nextFutureControllerReportSlot(origin, origin.Add(17*time.Millisecond), 4); got != 5 {
		t.Fatalf("next slot after late service = %d, want 5 (20ms)", got)
	}
	for _, tc := range []struct {
		now  time.Duration
		want uint64
	}{
		{now: 110 * time.Millisecond, want: 27},
		{now: time.Second, want: 250},
	} {
		if got, ok := latestElapsedControllerReportSlot(origin, origin.Add(tc.now), 0); !ok || got != tc.want {
			t.Fatalf("single late service at %s = %d, %v; want latest slot %d, true", tc.now, got, ok, tc.want)
		}
	}
}

func TestControllerReportClockTenSecondGridIsIndependentOfProducerCadence(t *testing.T) {
	const duration = 10 * time.Second
	origin := time.Unix(1, 0)
	const reportCount = uint64(duration / controllerReportPeriod)
	for _, producerPeriod := range []time.Duration{time.Millisecond, 4 * time.Millisecond, 8 * time.Millisecond} {
		for _, phase := range []time.Duration{0, producerPeriod / 3, 2 * producerPeriod / 3} {
			t.Run(producerPeriod.String()+"/"+phase.String(), func(t *testing.T) {
				// Producer updates only replace state. They do not participate in
				// slot arithmetic, so every modeled publication phase sees the
				// same device-owned 250 Hz grid.
				for update := phase; update < duration; update += producerPeriod {
					slot := nextFutureControllerReportSlot(origin, origin.Add(update), 0)
					if slot == 0 || controllerReportSlotTime(origin, slot).Sub(origin)%controllerReportPeriod != 0 {
						t.Fatalf("producer update at %s changed grid alignment (slot %d)", update, slot)
					}
				}

				for slot := uint64(1); slot <= reportCount; slot++ {
					due := controllerReportSlotTime(origin, slot)
					if got := due.Sub(origin); got != time.Duration(slot)*controllerReportPeriod {
						t.Fatalf("slot %d deadline = %s, want %s", slot, got, time.Duration(slot)*controllerReportPeriod)
					}
					if got := nextFutureControllerReportSlot(origin, due, slot-1); got != slot+1 {
						t.Fatalf("request at slot %d did not select strict future slot: got %d", slot, got)
					}
				}
			})
		}
	}
}

func TestControllerReportClockMatchesSDLFourMillisecondIntegrationStep(t *testing.T) {
	const gyroDegreesPerSecond = 250.0
	const reportsPerSecond = 250
	for _, submitRate := range []int{1000, 250, 125} {
		t.Run(time.Duration(time.Second/time.Duration(submitRate)).String(), func(t *testing.T) {
			producerPeriod := time.Second / time.Duration(submitRate)
			var submissions int
			var integratedDegrees float64
			latestGyro := 0.0
			nextUpdate := producerPeriod / 2 // exercise a non-grid producer phase
			var generatedReports int
			for slot := uint64(1); slot <= reportsPerSecond; slot++ {
				reportAt := time.Duration(slot) * controllerReportPeriod
				for nextUpdate <= reportAt && nextUpdate < time.Second {
					latestGyro = gyroDegreesPerSecond
					submissions++
					nextUpdate += producerPeriod
				}
				integratedDegrees += latestGyro * controllerReportPeriod.Seconds()
				generatedReports++
			}
			wantSubmissions := submitRate
			if submissions != wantSubmissions {
				t.Fatalf("modeled producer submissions = %d, want %d", submissions, wantSubmissions)
			}
			if generatedReports != 250 {
				t.Fatalf("device report count = %d, want 250", generatedReports)
			}
			if math.Abs(integratedDegrees-gyroDegreesPerSecond) > 1e-9 {
				t.Fatalf("%d producer submissions/s integrated %.6f degrees, want %.6f from 250 reports/s", submitRate, integratedDegrees, gyroDegreesPerSecond)
			}
		})
	}
}

func TestControllerReportClockSamplesLatestStateAtReportBoundary(t *testing.T) {
	type update struct {
		at    time.Duration
		value int
	}
	updates := []update{{at: 4*time.Millisecond - time.Nanosecond, value: 1}, {at: 4*time.Millisecond + time.Nanosecond, value: 2}}
	sample := func(at time.Duration) int {
		value := 0
		for _, update := range updates {
			if update.at > at {
				break
			}
			value = update.value
		}
		return value
	}
	if got := sample(4 * time.Millisecond); got != 1 {
		t.Fatalf("sample at 4ms = %d, want latest state submitted just before slot", got)
	}
	if got := sample(8 * time.Millisecond); got != 2 {
		t.Fatalf("sample at 8ms = %d, want latest state submitted after prior slot", got)
	}
	if got := len(updates); got != 2 {
		t.Fatalf("producer updates generated %d modeled reports, want none until clock slots", got)
	}
}
