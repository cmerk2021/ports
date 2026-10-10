package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/cmerk2021/ports/internal/app"
	"github.com/cmerk2021/ports/internal/audit"
	"github.com/cmerk2021/ports/internal/model"
)

func table(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func printStackInfo(w io.Writer, info *app.StackInfo) {
	s := info.Stack
	fmt.Fprintf(w, "Stack:      %s\n", s.Name)
	fmt.Fprintf(w, "Directory:  %s\n", s.Directory)
	fmt.Fprintf(w, "Manifest:   %s (%s)\n", info.ManifestPath, info.ManifestStatus)
	if len(s.ComposeFiles) > 0 {
		fmt.Fprintf(w, "Compose:    %s\n", strings.Join(s.ComposeFiles, ", "))
	}
	fmt.Fprintf(w, "Updated:    %s\n", s.UpdatedAt.Format("2006-01-02 15:04:05 MST"))

	fmt.Fprintln(w, "\nReservations:")
	if len(s.Allocations) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		t := table(w)
		fmt.Fprintln(t, "  TYPE\tPORTS\tPROTO\tBIND\tPURPOSE")
		for _, a := range s.Allocations {
			fmt.Fprintf(t, "  %s\t%s\t%s\t%s\t%s\n", a.Type, a.RangeString(), a.Protocol, a.BindAddress, dash(a.Purpose))
		}
		t.Flush()
	}

	fmt.Fprintln(w, "\nAssignments (intended, not necessarily published):")
	if len(s.Assignments) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		t := table(w)
		fmt.Fprintln(t, "  NAME\tHOST\tCONTAINER\tPROTO\tPURPOSE")
		for _, a := range s.Assignments {
			fmt.Fprintf(t, "  %s\t%s:%d\t%d\t%s\t%s\n", a.Name, a.BindAddress, a.HostPort, a.ContainerPort, a.Protocol, dash(a.Purpose))
		}
		t.Flush()
	}

	fmt.Fprintln(w, "\nCompose port mappings (effective configuration):")
	switch info.ComposeStatus {
	case app.ComposeSkipped:
		fmt.Fprintln(w, "  (not inspected)")
	case app.ComposeStatusOK:
		if len(info.Mappings) == 0 {
			fmt.Fprintln(w, "  (no published ports)")
		} else {
			t := table(w)
			fmt.Fprintln(t, "  SERVICE\tHOST\tCONTAINER\tPROTO")
			for _, m := range info.Mappings {
				fmt.Fprintf(t, "  %s\t%s:%d\t%d\t%s\n", m.Service, m.BindAddress, m.HostPort, m.ContainerPort, m.Protocol)
			}
			t.Flush()
		}
		for _, n := range info.ComposeNotes {
			fmt.Fprintf(w, "  note: %s%s\n", svcPrefix(n.Service), n.Message)
		}
	default:
		fmt.Fprintf(w, "  (%s: %s)\n", info.ComposeStatus, info.ComposeError)
	}

	if len(info.Discrepancies) > 0 {
		fmt.Fprintln(w, "\nDiscrepancies between registry and manifest (run 'ports sync --dry-run'):")
		for _, d := range info.Discrepancies {
			fmt.Fprintf(w, "  - %s\n", d)
		}
	}
}

func svcPrefix(s string) string {
	if s == "" {
		return ""
	}
	return "service " + s + ": "
}

func printList(w io.Writer, reg *model.Registry) {
	stacks := reg.SortedStacks()
	if len(stacks) == 0 {
		fmt.Fprintln(w, "No stacks registered. Register one with 'ports new <stack>'.")
		return
	}
	t := table(w)
	fmt.Fprintln(t, "STACK\tFIXED\tBLOCKS\tASSIGNED\tDIRECTORY")
	for _, s := range stacks {
		var fixed, blocks []string
		for _, a := range s.Allocations {
			if a.Type == model.TypeBlock {
				blocks = append(blocks, a.RangeString())
				continue
			}
			f := a.RangeString() + "/" + a.Protocol
			if a.BindAddress != model.WildcardIPv4 {
				f = a.BindAddress + ":" + f
			}
			fixed = append(fixed, f)
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%d\t%s\n", s.Name, dash(strings.Join(fixed, ",")), dash(strings.Join(blocks, ",")), len(s.Assignments), s.Directory)
	}
	t.Flush()
}

func printFind(w io.Writer, port int, matches []app.FindMatch) {
	for _, m := range matches {
		fmt.Fprintf(w, "%d: stack %s (%s)\n", port, m.Stack, m.Allocation)
		for _, as := range m.Assignments {
			fmt.Fprintf(w, "  assigned: %s\n", as)
		}
		if len(m.Assignments) == 0 {
			fmt.Fprintln(w, "  not assigned")
		}
	}
}

func printFinding(w io.Writer, f audit.Finding) {
	fmt.Fprintf(w, "  %-9s %-26s %s\n", strings.ToUpper(string(f.Severity)), f.Code, f.Message)
}

func printAudit(w io.Writer, r *audit.Report) {
	if len(r.Global) > 0 {
		fmt.Fprintln(w, "Global:")
		for _, f := range r.Global {
			printFinding(w, f)
		}
		fmt.Fprintln(w)
	}
	for _, s := range r.Stacks {
		fmt.Fprintf(w, "Stack %s (%s) - compose: %s, %d published mapping(s)\n", s.Stack, s.Directory, s.ComposeStatus, len(s.Mappings))
		if len(s.Findings) == 0 {
			fmt.Fprintln(w, "  OK")
		}
		for _, f := range s.Findings {
			printFinding(w, f)
		}
	}
	fmt.Fprintf(w, "\nSummary: %d violation(s), %d error(s), %d warning(s), %d info; host inspection: %s\n",
		r.Summary.Violations, r.Summary.Errors, r.Summary.Warnings, r.Summary.Info, r.HostScan)
	fmt.Fprintln(w, "Note: a clean audit does not guarantee ports are free at runtime; Docker's bind is authoritative.")
}

func printSync(w io.Writer, r *app.SyncReport) {
	if len(r.Items) == 0 {
		fmt.Fprintln(w, "Registry and manifests are consistent.")
		return
	}
	for _, it := range r.Items {
		name := it.Stack
		if name == "" {
			name = "(stack root)"
		}
		fmt.Fprintf(w, "%s: %s\n", name, it.Kind)
		for _, d := range it.Details {
			fmt.Fprintf(w, "    %s\n", d)
		}
		safety := "requires manual review"
		if it.Safe {
			safety = "safe"
		}
		fmt.Fprintf(w, "  proposed: %s [%s]\n", it.Action, safety)
		if it.Result != "" {
			fmt.Fprintf(w, "  result:   %s\n", it.Result)
		}
	}
	if r.DryRun {
		fmt.Fprintln(w, "\nDry run: nothing was changed. Re-run with --apply to perform the safe repairs.")
	}
}
