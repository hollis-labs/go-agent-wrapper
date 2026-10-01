// Package filters defines the integration point for the harness
// filter pipeline described in the companion architecture note
// "Harness Filters, Directives, And Normalization".
//
// The concrete filter rules live in go-harness-filters, which
// Nanite/Torque/Tether/Hadron can use directly without the wrapper.
// This package owns the wrapper-side contract so the wrapper can call
// into a pipeline when one is configured, and degrade to passthrough
// when one is not; [RepairPipeline] adapts go-harness-filters repair
// rules into a [Pipeline].
package filters
