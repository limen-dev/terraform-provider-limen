// Command terraform-provider-limen is the Terraform and OpenTofu provider
// for Limen.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/limen-dev/terraform-provider-limen/internal/provider"
)

// version is set by GoReleaser.
var version = "dev"

//go:generate go tool tfplugindocs generate --provider-name limen

func main() {
	debug := flag.Bool("debug", false, "run with support for debuggers like delve")
	flag.Parse()
	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/limen-dev/limen",
		Debug:   *debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
