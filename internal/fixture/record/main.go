// Command record records one fixture set against the throwaway services of this
// repository and publishes it under the runner's write root. The dev container
// runs it from the repository root; it prints one fixed reason word and stops,
// so a refused recording publishes nothing and the committed set is kept.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/ViktorsBaikers/Boxset/internal/fixture"
	"github.com/ViktorsBaikers/Boxset/internal/fixture/synthetic"
)

func main() {
	set := flag.String("set", "", "the fixture set to record, for example "+synthetic.Set)
	flag.Parse()
	if !fixture.ValidSet(*set) {
		refuse(fixture.ErrSetInvalid)
	}
	doc, err := synthetic.Record(context.Background(), *set)
	if err != nil {
		refuse(err)
	}
	if err := synthetic.Publish(doc); err != nil {
		refuse(err)
	}
	fmt.Printf("FIXTURE-RECORD set=%s exchanges=%d provenance=%s redistribution=%s\n",
		doc.Set, len(doc.Exchanges), doc.Provenance.Source, doc.Provenance.Redistribution)
}

// refuse prints the fixed reason word, never a request, a header or a body.
func refuse(err error) {
	reason := fixture.Token(err)
	if reason == "" {
		reason = "internal_error"
	}
	fmt.Fprintf(os.Stderr, "fixture-record: %s\n", reason)
	os.Exit(1)
}
