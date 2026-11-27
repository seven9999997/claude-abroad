package resolverlock

// PersistedFormats 是本包写进状态目录的落盘结构（文件名 → 结构的零值）。只给 internal/daemon 的 TestStateFormatPinned
// 用反射列出字段清单（v0.71：落盘结构有任何变化都要把 buildinfo.StateFormat 加一），运行时不用。
func PersistedFormats() map[string]any {
	return map[string]any{RecordFile: record{}}
}
