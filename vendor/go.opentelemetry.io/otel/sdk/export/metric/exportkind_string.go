// Code generated by "stringer -type=ExportKind"; DO NOT EDIT.

package metric // import "go.opentelemetry.io/otel/sdk/export/metric"

import "strconv"

func _() {
	// An "invalid array index" compiler error signifies that the constant values have changed.
	// Re-run the stringer command to generate them again.
	var x [1]struct{}
	_ = x[CumulativeExportKind-1]
	_ = x[DeltaExportKind-2]
}

const _ExportKind_name = "CumulativeExportKindDeltaExportKind"

var _ExportKind_index = [...]uint8{0, 20, 35}

func (i ExportKind) String() string {
	i -= 1
	if i < 0 || i >= ExportKind(len(_ExportKind_index)-1) {
		return "ExportKind(" + strconv.FormatInt(int64(i+1), 10) + ")"
	}
	return _ExportKind_name[_ExportKind_index[i]:_ExportKind_index[i+1]]
}