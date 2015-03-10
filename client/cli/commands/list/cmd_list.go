// Copyright 2015 Apcera Inc. All rights reserved.

package list

import (
	"encoding/json"
	"fmt"

	"github.com/apcera/continuum/apc/utils"
	"github.com/apcera/kurma/client/cli"
	"github.com/apcera/termtables"
	"github.com/appc/spec/schema"

	pb "github.com/apcera/kurma/stage1/client"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

func init() {
	cli.DefineCommand("list", parseFlags, list, cliList, "FIXME")
}

func parseFlags(cmd *cli.Cmd) {
}

func cliList(cmd *cli.Cmd) error {
	if len(cmd.Args) > 0 {
		return utils.NewUsageError("Invalid command options specified.")
	}
	return cmd.Run()
}

func list(cmd *cli.Cmd) error {
	conn, err := grpc.Dial("localhost:12311")
	if err != nil {
		return err
	}
	defer conn.Close()

	c := pb.NewKurmaClient(conn)
	resp, err := c.List(context.Background(), &pb.None{})
	if err != nil {
		return err
	}

	// create the table
	table := termtables.CreateTable()

	table.AddHeaders("UUID", "Name", "State")

	for _, container := range resp.Containers {
		var manifest *schema.ContainerRuntimeManifest
		if err := json.Unmarshal(container.Manifest, &manifest); err != nil {
			return err
		}
		var appName string
		for _, app := range manifest.Apps {
			appName = app.Name.String()
			break
		}
		table.AddRow(container.Uuid, appName, container.State.String())
	}
	fmt.Printf("%s", table.Render())
	return nil
}
