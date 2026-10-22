package steamdeck

import "time"

func nextFutureControllerReportSlot(origin, now time.Time, lastServed uint64) uint64 {
	elapsed := now.Sub(origin)
	slot := uint64(1)
	if elapsed >= 0 {
		// Strictly future: a request arriving on a grid boundary targets the
		// following boundary, not a slot that has already arrived.
		slot = uint64(elapsed/controllerReportPeriod) + 1
	}
	if slot <= lastServed {
		slot = lastServed + 1
	}
	return slot
}

func latestElapsedControllerReportSlot(origin, now time.Time, lastServed uint64) (uint64, bool) {
	elapsed := now.Sub(origin)
	if elapsed < controllerReportPeriod {
		return 0, false
	}
	slot := uint64(elapsed / controllerReportPeriod)
	if slot <= lastServed {
		return 0, false
	}
	return slot, true
}

func controllerReportSlotTime(origin time.Time, slot uint64) time.Time {
	return origin.Add(time.Duration(slot) * controllerReportPeriod)
}
