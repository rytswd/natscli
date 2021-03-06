// Copyright 2020 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/xlab/tablewriter"
	"gopkg.in/alecthomas/kingpin.v2"
)

type SrvLsCmd struct {
	expect  uint32
	json    bool
	filter  string
	sort    string
	reverse bool
}

type srvListCluster struct {
	name  string
	nodes []string
	gwOut int
	gwIn  int
	conns int
}

func configureServerListCommand(srv *kingpin.CmdClause) {
	c := &SrvLsCmd{}

	ls := srv.Command("list", "List known servers").Alias("ls").Action(c.list)
	ls.Arg("expect", "How many servers to expect").Uint32Var(&c.expect)
	ls.Flag("json", "Produce JSON output").Short('j').BoolVar(&c.json)
	ls.Flag("filter", "Regular expression filter on server name").Short('f').StringVar(&c.filter)
	ls.Flag("sort", "Sort servers by a specific key (conns,subs,routes,gws,mem,cpu,slow,uptime,rtt").Default("rtt").EnumVar(&c.sort, strings.Split("conns,conn,subs,sub,routes,route,gw,mem,cpu,slow,uptime,rtt", ",")...)
	ls.Flag("reverse", "Reverse sort servers").Short('R').BoolVar(&c.reverse)
}

func (c *SrvLsCmd) list(_ *kingpin.ParseContext) error {
	nc, err := newNatsConn("", natsOpts()...)
	if err != nil {
		return err
	}
	defer nc.Close()

	ec, err := nats.NewEncodedConn(nc, nats.JSON_ENCODER)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	seen := uint32(0)
	mu := sync.Mutex{}

	type result struct {
		*server.ServerStatsMsg
		rtt time.Duration
	}

	var results []*result
	filter, err := regexp.Compile(c.filter)
	if err != nil {
		return err
	}

	var (
		clusters           = make(map[string]*srvListCluster)
		servers            = 0
		connections        = 0
		memory      int64  = 0
		slow        int64  = 0
		subs        uint32 = 0
		start              = time.Now()
	)

	sub, err := ec.Subscribe(nc.NewRespInbox(), func(ssm *server.ServerStatsMsg) {
		mu.Lock()
		defer mu.Unlock()

		last := atomic.AddUint32(&seen, 1)

		if !filter.MatchString(ssm.Server.Name) {
			return
		}

		servers++
		connections += ssm.Stats.Connections
		memory += ssm.Stats.Mem
		slow += ssm.Stats.SlowConsumers
		subs += ssm.Stats.NumSubs

		cluster := ssm.Server.Cluster
		if cluster != "" {
			_, ok := clusters[cluster]
			if !ok {
				clusters[cluster] = &srvListCluster{cluster, []string{}, 0, 0, 0}
			}

			clusters[cluster].conns += ssm.Stats.Connections
			clusters[cluster].nodes = append(clusters[cluster].nodes, ssm.Server.Name)
			clusters[cluster].gwOut += len(ssm.Stats.Gateways)
			for _, g := range ssm.Stats.Gateways {
				clusters[cluster].gwIn += g.NumInbound
			}
		}

		results = append(results, &result{
			ServerStatsMsg: ssm,
			rtt:            time.Since(start),
		})

		if last == c.expect {
			cancel()
		}
	})
	if err != nil {
		return err
	}

	err = nc.PublishRequest("$SYS.REQ.SERVER.PING", sub.Subject, nil)
	if err != nil {
		return err
	}

	ic := make(chan os.Signal, 1)
	signal.Notify(ic, os.Interrupt)

	select {
	case <-ic:
		cancel()
	case <-ctx.Done():
	}

	sub.Drain()

	if len(results) == 0 {
		return fmt.Errorf("no results received, ensure the account used has system privileges and appropriate permissions")
	}

	if c.json {
		printJSON(results)
		return nil
	}

	table := tablewriter.CreateTable()
	table.AddTitle("Server Overview")
	table.AddHeaders("Name", "Cluster", "IP", "Version", "Conns", "Subs", "Routes", "GWs", "Mem", "CPU", "Slow", "Uptime", "RTT")

	rev := func(v bool) bool {
		if c.reverse {
			return !v
		}
		return v
	}

	sort.Slice(results, func(i int, j int) bool {
		stati := results[i].Stats
		statj := results[j].Stats

		switch c.sort {
		case "conns", "conn":
			return rev(stati.Connections < statj.Connections)
		case "subs", "sub":
			return rev(stati.NumSubs < statj.NumSubs)
		case "routes", "route":
			return rev(len(stati.Routes) < len(statj.Routes))
		case "gws", "gw":
			return rev(len(stati.Gateways) < len(statj.Gateways))
		case "mem":
			return rev(stati.Mem < statj.Mem)
		case "cpu":
			return rev(stati.CPU < statj.CPU)
		case "slow":
			return rev(stati.SlowConsumers < statj.SlowConsumers)
		case "uptime":
			return rev(stati.Start.UnixNano() > statj.Start.UnixNano())
		default:
			return rev(results[i].rtt < results[j].rtt)
		}
	})

	for _, ssm := range results {
		table.AddRow(ssm.Server.Name, ssm.Server.Cluster, ssm.Server.Host, ssm.Server.Version, ssm.Stats.Connections, ssm.Stats.NumSubs, len(ssm.Stats.Routes), len(ssm.Stats.Gateways), humanize.IBytes(uint64(ssm.Stats.Mem)), fmt.Sprintf("%.1f", ssm.Stats.CPU), ssm.Stats.SlowConsumers, humanizeTime(ssm.Stats.Start), ssm.rtt)
	}

	table.AddSeparator()
	table.AddRow("", fmt.Sprintf("%d Clusters", len(clusters)), fmt.Sprintf("%d Servers", servers), "", connections, subs, "", "", humanize.IBytes(uint64(memory)), "", slow, "", "")
	fmt.Print(table.Render())

	if c.expect != 0 && c.expect != seen {
		fmt.Printf("\nMissing %d server(s)\n", c.expect-atomic.LoadUint32(&seen))
	}

	if len(clusters) > 0 {
		c.showClusters(clusters)
	}

	return nil
}

func (c *SrvLsCmd) showClusters(cl map[string]*srvListCluster) {
	fmt.Println()
	table := tablewriter.CreateTable()
	table.AddTitle("Cluster Overview")
	table.AddHeaders("Cluster", "Node Count", "Outgoing Gateways", "Incoming Gateways", "Connections")

	var clusters []*srvListCluster
	for c := range cl {
		clusters = append(clusters, cl[c])
	}

	sort.Slice(clusters, func(i, j int) bool {
		return len(clusters[i].nodes) > len(clusters[j].nodes)
	})

	in := 0
	out := 0
	nodes := 0
	conns := 0

	for _, c := range clusters {
		in += c.gwIn
		out += c.gwOut
		nodes += len(c.nodes)
		conns += c.conns
		table.AddRow(c.name, len(c.nodes), c.gwOut, c.gwIn, c.conns)
	}
	table.AddSeparator()
	table.AddRow("", nodes, out, in, conns)

	fmt.Print(table.Render())
}
