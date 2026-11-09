package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"polygate/internal/domain"
	"polygate/internal/kubeobjects"
)

func main() {
	account := flag.String("account", "", "canonical account UUID")
	app := flag.String("app", "", "canonical app UUID; omit for tenant resources only")
	image := flag.String("image-digest", "", "platform-approved image digest")
	specFile := flag.String("spec", "", "JSON file with an AppSpec for a trusted workload")
	flag.Parse()
	objects, err := kubeobjects.Tenant(*account, domain.StudentQuota)
	if err != nil {
		fatal(err)
	}
	if *app != "" {
		if *image == "" || *specFile == "" {
			fatal(fmt.Errorf("-app requires -image-digest and -spec"))
		}
		file, err := os.Open(*specFile)
		if err != nil {
			fatal(err)
		}
		defer file.Close()
		var spec domain.AppSpec
		decoder := json.NewDecoder(file)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&spec); err != nil {
			fatal(err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			fatal(fmt.Errorf("spec file must contain exactly one JSON object"))
		}
		workload, err := kubeobjects.TrustedWorkload(*account, *app, spec, *image)
		if err != nil {
			fatal(err)
		}
		objects.Items = append(objects.Items, workload.Items...)
	}
	data, err := objects.JSON()
	if err != nil {
		fatal(err)
	}
	_, _ = os.Stdout.Write(append(data, '\n'))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
