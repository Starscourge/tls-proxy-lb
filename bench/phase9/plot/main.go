// Command plot turns the raw results of the Phase 9 experiments into SVG
// graphs and the summaries the README quotes. It uses the standard library
// only.
//
//	plot clock -save F | -check F
//	plot kill  -requests R -events E -health H -backend ADDR -name NAME -svg OUT
//	plot slow  -fault F -backend ADDR -name NAME -svg OUT name=dir ...
//	plot bench -dir DIR
//	plot variants -dir DIR -title T -svg NAME [-order a,b,...]
//
// Each subcommand prints its summary to stdout, writes its graph, and
// exits 1 when the run broke a claim the experiment makes (a failed
// request after an ejection, say), so the scripts that call it fail too.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: plot clock|kill|slow|bench|variants [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "clock":
		err = cmdClock(os.Args[2:])
	case "kill":
		err = cmdKill(os.Args[2:])
	case "slow":
		err = cmdSlow(os.Args[2:])
	case "bench":
		err = cmdBench(os.Args[2:])
	case "variants":
		err = cmdVariants(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "plot: unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "plot:", err)
		os.Exit(1)
	}
}
