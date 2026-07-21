package main

import "github.com/marmutapp/superbased-observer/internal/benchmark"

// benchmark_export.go bridges the CLI to the ONE canonical-JSON serializer,
// which now lives in the pure internal/benchmark package (benchmark.BuildExport)
// so the dashboard export endpoint shares the exact same shape (plan §4.3). The
// CLI keeps these local names so the report/export verbs read unchanged.

const (
	benchmarkExportSchema = benchmark.ExportSchema
	priceDisclaimer       = benchmark.PriceDisclaimer
)

// buildBenchmarkExport delegates to the single serialization owner.
func buildBenchmarkExport(run benchmark.RunRecord, rep benchmark.Report, generatedAt string) benchmark.Export {
	return benchmark.BuildExport(run, rep, generatedAt)
}
