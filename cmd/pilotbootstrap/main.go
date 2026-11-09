package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"polygate/internal/domain"
	"polygate/internal/kubeobjects"
)

// pilotbootstrap writes an operator-reviewed manifest. It never contacts the
// cluster or handles Kubernetes credentials.
func main() {
	accountsFile := flag.String("accounts-file", "", "text file containing one canonical account UUID per line")
	flag.Parse()
	if *accountsFile == "" {
		fail("-accounts-file is required")
	}
	file, err := os.Open(*accountsFile)
	if err != nil {
		fail(err.Error())
	}
	defer file.Close()
	var ids []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		id := strings.TrimSpace(scanner.Text())
		if id != "" {
			ids = append(ids, id)
		}
	}
	if err := scanner.Err(); err != nil {
		fail(err.Error())
	}
	objects, err := kubeobjects.PrecreatedTenants(ids, domain.StudentQuota)
	if err != nil {
		fail(err.Error())
	}
	data, err := objects.JSON()
	if err != nil {
		fail(err.Error())
	}
	_, _ = os.Stdout.Write(append(data, '\n'))
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
