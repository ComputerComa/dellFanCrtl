// Package classify assigns a logical SensorClass to a raw sensor name so
// discovery can group and curve sensors sensibly without the user having to
// hand-classify dozens of entries.
package classify

import (
	"regexp"
	"strconv"
	"strings"

	"dellfanctl/internal/ipmi"
	"dellfanctl/internal/model"
)

var rules = []struct {
	re    *regexp.Regexp
	class model.SensorClass
}{
	{regexp.MustCompile(`(?i)inlet`), model.ClassInlet},
	{regexp.MustCompile(`(?i)exhaust|outlet`), model.ClassExhaust},
	{regexp.MustCompile(`(?i)^cpu\s*\d*\s*temp`), model.ClassCPU},
	{regexp.MustCompile(`(?i)fan.*rpm|^fan\s*\d`), model.ClassFanRPM},
	{regexp.MustCompile(`(?i)dimm|memory|^mem\b`), model.ClassMemory},
	{regexp.MustCompile(`(?i)psu|power supply`), model.ClassPSU},
	{regexp.MustCompile(`(?i)usage|utilization`), model.ClassUtilization},
	{regexp.MustCompile(`(?i)current|voltage|volt|watt|amps?\b|pwr consumption`), model.ClassPower},
}

// IPMISensor classifies an IPMI sensor by name, unit, and (if known)
// IPMI entity id.instance. A generic "Temp" name on a Processor entity
// (e.g. "3.1") is recognized as a per-CPU temperature even though the name
// alone gives no hint — common on Dell PowerEdge boards where both CPU
// sensors are simply named "Temp".
func IPMISensor(name, unit, entity string) model.SensorClass {
	for _, r := range rules {
		if r.re.MatchString(name) {
			return r.class
		}
	}
	if strings.HasPrefix(entity, ipmi.EntityProcessorPrefix) && strings.Contains(strings.ToLower(unit), "degrees") {
		return model.ClassCPU
	}
	if strings.Contains(strings.ToLower(unit), "degrees") {
		return model.ClassBoard
	}
	return model.ClassOther
}

// DisplayName derives a friendlier name for sensors whose raw IPMI name is
// too generic to distinguish (e.g. two sensors both literally named "Temp").
// occurrence is the 1-based count of sensors sharing that class+name seen so
// far, used to number them ("CPU1 Temp", "CPU2 Temp").
func DisplayName(rawName string, class model.SensorClass, occurrence int) string {
	if class == model.ClassCPU && strings.EqualFold(strings.TrimSpace(rawName), "temp") {
		return "CPU" + strconv.Itoa(occurrence) + " Temp"
	}
	return rawName
}
