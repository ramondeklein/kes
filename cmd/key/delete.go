package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"

	"github.com/aead/key"
)

const deleteCmdUsage = `usage: %s name

  --tls-skip-verify    Skip X.509 certificate validation during TLS handshake

  -h, --help           Show list of command-line options
`

func deleteKey(args []string) {
	cli := flag.NewFlagSet(args[0], flag.ExitOnError)
	cli.Usage = func() {
		fmt.Fprintf(cli.Output(), deleteCmdUsage, cli.Name())
	}

	var insecureSkipVerify bool
	cli.BoolVar(&insecureSkipVerify, "tls-skip-verify", false, "Skip X.509 certificate validation during TLS handshake")

	cli.Parse(args[1:])
	if args = cli.Args(); len(args) != 1 {
		cli.Usage()
		os.Exit(2)
	}

	name := args[0]
	client := key.NewClient(serverAddr(), &tls.Config{
		InsecureSkipVerify: insecureSkipVerify,
		Certificates:       loadClientCertificates(),
	})
	if err := client.DeleteKey(name); err != nil {
		failf(cli.Output(), "Failed to delete %s: %s", name, err.Error())
	}
}