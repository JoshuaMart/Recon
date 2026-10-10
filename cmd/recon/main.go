// Command recon is the operational command line. It holds the owner credential,
// which is why it is not the control plane or a route: everything here is something
// a person does to a deployment.
//
// The migrator stays its own binary: a failed migration must be readable alone, and
// the deployment gates the application on it rather than on a remembered ordering.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5"

	"github.com/JoshuaMart/recon/internal/bootstrap"
	"github.com/JoshuaMart/recon/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "recon: %v\n", err)
		os.Exit(1)
	}
}

func usage() error {
	return errors.New("usage: recon bootstrap --org NAME --email ADDRESS [--token]")
}

func run() error {
	if len(os.Args) < 2 {
		return usage()
	}

	switch os.Args[1] {
	case "bootstrap":
		return runBootstrap(os.Args[2:])
	default:
		return usage()
	}
}

func runBootstrap(args []string) error {
	flags := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	org := flags.String("org", "", "name of the organization to create")
	email := flags.String("email", "", "email address of its first member")
	tokenName := flags.String("token-name", "bootstrap", "label of the token in api_token")
	// On an existing account this is the only reason to run again: the first secret was
	// printed once and never stored.
	mint := flags.Bool("token", false, "mint another token for an account that already exists")
	if err := flags.Parse(args); err != nil {
		return err
	}

	// The owner's configuration, the migrator's. Asking for the application role would
	// put the command back on the path this design keeps it off, and the same
	// validation refuses a process holding both credentials.
	cfg, err := config.Load(config.RoleMigrate, config.Options{})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// One connection, not a pool: this runs once and commits once.
	conn, err := pgx.Connect(ctx, cfg.Database.MigrationURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	result, err := bootstrap.Run(ctx, conn, bootstrap.Request{
		Org:       *org,
		Email:     *email,
		TokenName: *tokenName,
		MintToken: *mint,
	})
	if err != nil {
		if errors.Is(err, bootstrap.ErrNoOrg) || errors.Is(err, bootstrap.ErrNoEmail) {
			flags.Usage()
		}
		return err
	}

	report(result)
	return nil
}

// report says what happened, not what was asked for.
func report(result bootstrap.Result) {
	if result.OrgCreated {
		fmt.Printf("organization created  %s\n", result.OrgID)
	} else {
		fmt.Printf("organization found    %s\n", result.OrgID)
	}
	if result.UserCreated {
		fmt.Printf("user created          %s\n", result.UserID)
	} else {
		fmt.Printf("user found            %s\n", result.UserID)
	}

	if result.Token == "" {
		fmt.Println("\nNo token minted. Pass -token to add one.")
		return
	}
	// Printed once, as only its hash is stored. Saying so costs a line and saves a
	// conversation.
	fmt.Printf("\ntoken, shown once, only its hash is stored:\n\n    %s\n\n", result.Token)
	fmt.Println("Paste it into the console. Revoke it by setting api_token.revoked_at.")
}
