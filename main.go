package main

import (
        "context"
        "encoding/json"
        "fmt"
        "io"
        "net"
        "net/http"
        "os"
        "os/signal"
        "strconv"
        "strings"
        "syscall"
        "time"

        "github.com/rs/zerolog"
        "github.com/rs/zerolog/log"
        "github.com/spf13/cobra"

        "github.com/gydschain/litenode/config"
        "github.com/gydschain/litenode/consensus"
        "github.com/gydschain/litenode/core"
        "github.com/gydschain/litenode/p2p"
        "github.com/gydschain/litenode/rpc"
)

var version = "1.0.0"

func main() {
        root := &cobra.Command{
                Use:   "gyds-boostnode",
                Short: "GYDS Chain Boost Node",
                Long: `GYDS Boostnode — a lightweight blockchain node for the GYDS Chain.
Supports light sync, RPC API, WebSocket subscriptions, and P2P networking.`,
        }

        root.AddCommand(startCmd(), genesisCmd(), versionCmd(), healthCmd(), peersCmd())
        if err := root.Execute(); err != nil {
                os.Exit(1)
        }
}

func startCmd() *cobra.Command {
        return &cobra.Command{
                Use:   "start",
                Short: "Start the GYDS boostnode",
                RunE: func(cmd *cobra.Command, args []string) error {
                        return runNode()
                },
        }
}

func genesisCmd() *cobra.Command {
        return &cobra.Command{
                Use:   "genesis",
                Short: "Print genesis block",
                Run: func(cmd *cobra.Command, args []string) {
                        b := core.GenesisBlock(core.GydsGenesis)
                        enc := json.NewEncoder(os.Stdout)
                        enc.SetIndent("", "  ")
                        enc.Encode(b.ToMap())
                },
        }
}

func versionCmd() *cobra.Command {
        return &cobra.Command{
                Use:   "version",
                Short: "Print version",
                Run: func(cmd *cobra.Command, args []string) {
                        fmt.Printf("gyds-boostnode v%s\n", version)
                },
        }
}

// ── Health command ─────────────────────────────────────────────────────────────

func healthCmd() *cobra.Command {
        var (
                host       string
                port       int
                timeoutSec int
                jsonOut    bool
        )

        cmd := &cobra.Command{
                Use:   "health",
                Short: "Check the health of a running boostnode",
                Long: `Query the local RPC server and print node health status.

Exit codes:
  0  — node is reachable and healthy
  1  — node is unreachable or reported unhealthy

Examples:
  gyds-boostnode health
  gyds-boostnode health --port 8545
  gyds-boostnode health --json
  gyds-boostnode health --host 192.168.1.10 --port 8545 --timeout 10`,
                RunE: func(cmd *cobra.Command, args []string) error {
                        return runHealth(host, port, timeoutSec, jsonOut)
                },
        }

        // Defaults: respect env vars, then fall back to hardcoded defaults
        defaultHost := envOrDefault("GYDS_RPC_HOST", "127.0.0.1")
        defaultPort := envIntOrDefault("GYDS_RPC_PORT", 8545)

        cmd.Flags().StringVar(&host, "host", defaultHost, "RPC host to query")
        cmd.Flags().IntVarP(&port, "port", "p", defaultPort, "RPC port to query")
        cmd.Flags().IntVar(&timeoutSec, "timeout", 5, "Request timeout in seconds")
        cmd.Flags().BoolVar(&jsonOut, "json", false, "Output result as JSON (for scripting)")

        return cmd
}

type healthResult struct {
        Reachable   bool              `json:"reachable"`
        Status      string            `json:"status"`
        BlockHeight uint64            `json:"block_height"`
        ChainID     interface{}       `json:"chain_id"`
        NetworkName string            `json:"network_name,omitempty"`
        TotalBlocks interface{}       `json:"total_blocks,omitempty"`
        HeadHash    string            `json:"head_hash,omitempty"`
        Endpoint    string            `json:"endpoint"`
        LatencyMs   int64             `json:"latency_ms"`
        Error       string            `json:"error,omitempty"`
        Extra       map[string]interface{} `json:"extra,omitempty"`
}

func runHealth(host string, port int, timeoutSec int, jsonOut bool) error {
        base := fmt.Sprintf("http://%s:%d", host, port)
        timeout := time.Duration(timeoutSec) * time.Second
        client := &http.Client{Timeout: timeout}

        result := healthResult{
                Endpoint: base,
        }

        // ── /health ──────────────────────────────────────────────────────────────
        start := time.Now()
        healthData, err := getJSON(client, base+"/health")
        result.LatencyMs = time.Since(start).Milliseconds()

        if err != nil {
                result.Reachable = false
                result.Status = "unreachable"
                result.Error = err.Error()

                if jsonOut {
                        return printJSON(result)
                }
                printHealthTable(result, false)
                return fmt.Errorf("node unreachable: %w", err)
        }

        result.Reachable = true

        // Parse height from /health
        if v, ok := healthData["height"]; ok {
                switch h := v.(type) {
                case float64:
                        result.BlockHeight = uint64(h)
                }
        }
        if s, ok := healthData["status"].(string); ok {
                result.Status = s
        } else {
                result.Status = "ok"
        }

        // ── /api/status ───────────────────────────────────────────────────────────
        statusData, err := getJSON(client, base+"/api/status")
        if err == nil {
                if v, ok := statusData["chainId"]; ok {
                        result.ChainID = v
                }
                if v, ok := statusData["networkName"].(string); ok {
                        result.NetworkName = v
                }
                if v, ok := statusData["totalBlocks"]; ok {
                        result.TotalBlocks = v
                }
                if v, ok := statusData["headHash"].(string); ok {
                        result.HeadHash = v
                }
                if v, ok := statusData["blockHeight"]; ok {
                        switch h := v.(type) {
                        case float64:
                                result.BlockHeight = uint64(h)
                        }
                }
        }

        healthy := result.Status == "ok"

        if jsonOut {
                return printJSON(result)
        }
        printHealthTable(result, healthy)

        if !healthy {
                return fmt.Errorf("node reported status: %s", result.Status)
        }
        return nil
}

func printHealthTable(r healthResult, healthy bool) {
        // ANSI colours — skip if not a terminal
        green  := "\033[0;32m"
        red    := "\033[0;31m"
        yellow := "\033[1;33m"
        cyan   := "\033[0;36m"
        bold   := "\033[1m"
        reset  := "\033[0m"
        if !isTerminal() {
                green, red, yellow, cyan, bold, reset = "", "", "", "", "", ""
        }

        fmt.Println()
        fmt.Printf("%s%s══ GYDS Boost Node Health ══%s\n", bold, cyan, reset)
        fmt.Println()

        if !r.Reachable {
                fmt.Printf("  %s✗  Status      %s unreachable%s\n", red, reset, reset)
                fmt.Printf("  %s   Endpoint    %s %s%s\n", red, reset, r.Endpoint, reset)
                if r.Error != "" {
                        fmt.Printf("  %s   Error       %s %s%s\n", red, reset, r.Error, reset)
                }
                fmt.Println()
                return
        }

        statusIcon := green + "✓"
        statusText := green + r.Status
        if r.Status != "ok" {
                statusIcon = yellow + "!"
                statusText = yellow + r.Status
        }

        fmt.Printf("  %s  Status      %s %s%s\n", statusIcon, reset, statusText, reset)
        fmt.Printf("  %s  Endpoint    %s %s%s\n", statusIcon, reset, r.Endpoint, reset)
        fmt.Printf("  %s  Latency     %s %dms%s\n", statusIcon, reset, r.LatencyMs, reset)
        fmt.Printf("  %s  Block       %s #%d%s\n", statusIcon, reset, r.BlockHeight, reset)

        if r.ChainID != nil {
                fmt.Printf("  %s  Chain ID    %s %v%s\n", statusIcon, reset, r.ChainID, reset)
        }
        if r.NetworkName != "" {
                fmt.Printf("  %s  Network     %s %s%s\n", statusIcon, reset, r.NetworkName, reset)
        }
        if r.HeadHash != "" {
                hash := r.HeadHash
                if len(hash) > 20 {
                        hash = hash[:20] + "..."
                }
                fmt.Printf("  %s  Head Hash   %s %s%s\n", statusIcon, reset, hash, reset)
        }
        if r.TotalBlocks != nil {
                fmt.Printf("  %s  Total Blks  %s %v%s\n", statusIcon, reset, r.TotalBlocks, reset)
        }

        fmt.Println()
}

func printJSON(v interface{}) error {
        enc := json.NewEncoder(os.Stdout)
        enc.SetIndent("", "  ")
        return enc.Encode(v)
}

func getJSON(client *http.Client, url string) (map[string]interface{}, error) {
        resp, err := client.Get(url)
        if err != nil {
                return nil, err
        }
        defer resp.Body.Close()

        if resp.StatusCode < 200 || resp.StatusCode >= 300 {
                return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
        }

        body, err := io.ReadAll(resp.Body)
        if err != nil {
                return nil, err
        }

        var out map[string]interface{}
        if err := json.Unmarshal(body, &out); err != nil {
                return nil, fmt.Errorf("invalid JSON response: %w", err)
        }
        return out, nil
}

func isTerminal() bool {
        fi, err := os.Stdout.Stat()
        if err != nil {
                return false
        }
        return (fi.Mode() & os.ModeCharDevice) != 0
}

func envOrDefault(key, def string) string {
        if v := os.Getenv(key); v != "" {
                return v
        }
        return def
}

func envIntOrDefault(key string, def int) int {
        if v := os.Getenv(key); v != "" {
                if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
                        return n
                }
        }
        return def
}

// ── Peers command ─────────────────────────────────────────────────────────────

func peersCmd() *cobra.Command {
        var (
                host       string
                port       int
                timeoutSec int
                jsonOut    bool
                watch      bool
        )

        cmd := &cobra.Command{
                Use:   "peers",
                Short: "List connected P2P peers of a running boostnode",
                Long: `Query the local RPC server and display all connected P2P peers.

Exit codes:
  0  — node reachable (even if peer count is zero)
  1  — node unreachable

Examples:
  gyds-boostnode peers
  gyds-boostnode peers --json
  gyds-boostnode peers --port 8545 --watch`,
                RunE: func(cmd *cobra.Command, args []string) error {
                        return runPeers(host, port, timeoutSec, jsonOut, watch)
                },
        }

        defaultHost := envOrDefault("GYDS_RPC_HOST", "127.0.0.1")
        defaultPort := envIntOrDefault("GYDS_RPC_PORT", 8545)

        cmd.Flags().StringVar(&host, "host", defaultHost, "RPC host to query")
        cmd.Flags().IntVarP(&port, "port", "p", defaultPort, "RPC port to query")
        cmd.Flags().IntVar(&timeoutSec, "timeout", 5, "Request timeout in seconds")
        cmd.Flags().BoolVar(&jsonOut, "json", false, "Output result as JSON (for scripting)")
        cmd.Flags().BoolVarP(&watch, "watch", "w", false, "Refresh peer list every 5 seconds (Ctrl-C to stop)")

        return cmd
}

type peerEntry struct {
        Address     string `json:"address"`
        ChainID     int64  `json:"chain_id"`
        Height      uint64 `json:"height"`
        NodeMode    string `json:"node_mode"`
        Version     string `json:"version"`
        ConnectedAt string `json:"connected_at"`
        UptimeSec   int64  `json:"uptime_sec"`
        LatencyMs   int64  `json:"latency_ms"`
}

type peersResult struct {
        Reachable bool        `json:"reachable"`
        Endpoint  string      `json:"endpoint"`
        Count     int         `json:"count"`
        Peers     []peerEntry `json:"peers"`
        Error     string      `json:"error,omitempty"`
}

func runPeers(host string, port, timeoutSec int, jsonOut, watch bool) error {
        base := fmt.Sprintf("http://%s:%d", host, port)
        client := &http.Client{Timeout: time.Duration(timeoutSec) * time.Second}

        do := func() (peersResult, error) {
                res := peersResult{Endpoint: base}

                data, err := getJSON(client, base+"/api/peers")
                if err != nil {
                        res.Reachable = false
                        res.Error = err.Error()
                        return res, err
                }
                res.Reachable = true

                // Parse peers array
                raw, _ := json.Marshal(data["peers"])
                var rows []map[string]interface{}
                _ = json.Unmarshal(raw, &rows)

                entries := make([]peerEntry, 0, len(rows))
                for _, r := range rows {
                        e := peerEntry{
                                Address:     strVal(r, "address"),
                                NodeMode:    strVal(r, "node_mode"),
                                Version:     strVal(r, "version"),
                                ConnectedAt: strVal(r, "connected_at"),
                        }
                        if v, ok := r["chain_id"].(float64); ok {
                                e.ChainID = int64(v)
                        }
                        if v, ok := r["height"].(float64); ok {
                                e.Height = uint64(v)
                        }
                        if v, ok := r["uptime_sec"].(float64); ok {
                                e.UptimeSec = int64(v)
                        }
                        // Measure TCP latency to the peer address
                        e.LatencyMs = probeTCPLatency(e.Address, time.Duration(timeoutSec)*time.Second)
                        entries = append(entries, e)
                }
                res.Peers = entries
                res.Count = len(entries)
                return res, nil
        }

        if !watch {
                res, err := do()
                if jsonOut {
                        return printJSON(res)
                }
                printPeersTable(res)
                return err
        }

        // Watch mode — refresh every 5 s
        ticker := time.NewTicker(5 * time.Second)
        defer ticker.Stop()
        sig := make(chan os.Signal, 1)
        signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

        for {
                res, _ := do()
                if jsonOut {
                        printJSON(res) //nolint:errcheck
                } else {
                        // Clear screen between refreshes
                        fmt.Print("\033[H\033[2J")
                        printPeersTable(res)
                        fmt.Printf("  Refreshing every 5s — press Ctrl-C to stop\n\n")
                }
                select {
                case <-sig:
                        return nil
                case <-ticker.C:
                }
        }
}

func printPeersTable(r peersResult) {
        green  := "\033[0;32m"
        red    := "\033[0;31m"
        yellow := "\033[1;33m"
        cyan   := "\033[0;36m"
        dim    := "\033[2m"
        bold   := "\033[1m"
        reset  := "\033[0m"
        if !isTerminal() {
                green, red, yellow, cyan, dim, bold, reset = "", "", "", "", "", "", ""
        }

        fmt.Println()
        fmt.Printf("%s%s══ GYDS Boost Node — Peers ══%s\n", bold, cyan, reset)
        fmt.Println()

        if !r.Reachable {
                fmt.Printf("  %s✗  Node unreachable: %s%s\n\n", red, r.Error, reset)
                return
        }

        fmt.Printf("  %sEndpoint:%s  %s\n", dim, reset, r.Endpoint)
        fmt.Printf("  %sPeers:   %s  %d connected\n\n", dim, reset, r.Count)

        if r.Count == 0 {
                fmt.Printf("  %s(no peers connected)%s\n\n", yellow, reset)
                return
        }

        // Table header
        fmt.Printf("  %s%-26s  %-8s  %-8s  %-10s  %-9s  %s%s\n",
                bold, "ADDRESS", "CHAIN", "BLOCK#", "MODE", "LATENCY", "UPTIME", reset)
        fmt.Printf("  %s%s%s\n", dim, strings.Repeat("─", 80), reset)

        for _, p := range r.Peers {
                addr := p.Address
                if len(addr) > 25 {
                        addr = addr[:22] + "..."
                }

                latStr := fmt.Sprintf("%dms", p.LatencyMs)
                if p.LatencyMs < 0 {
                        latStr = "n/a"
                }

                uptimeStr := formatUptime(p.UptimeSec)

                latColour := green
                if p.LatencyMs > 200 {
                        latColour = yellow
                } else if p.LatencyMs < 0 {
                        latColour = red
                }

                fmt.Printf("  %-26s  %-8d  %-8d  %-10s  %s%-9s%s  %s\n",
                        addr, p.ChainID, p.Height, p.NodeMode,
                        latColour, latStr, reset,
                        uptimeStr)
        }
        fmt.Println()
}

// probeTCPLatency attempts a TCP dial and returns round-trip time in ms.
// Returns -1 if the address is empty or unreachable within timeout.
func probeTCPLatency(addr string, timeout time.Duration) int64 {
        if addr == "" {
                return -1
        }
        start := time.Now()
        conn, err := net.DialTimeout("tcp", addr, timeout)
        if err != nil {
                return -1
        }
        conn.Close()
        return time.Since(start).Milliseconds()
}

func formatUptime(sec int64) string {
        switch {
        case sec < 60:
                return fmt.Sprintf("%ds", sec)
        case sec < 3600:
                return fmt.Sprintf("%dm%ds", sec/60, sec%60)
        default:
                h := sec / 3600
                m := (sec % 3600) / 60
                return fmt.Sprintf("%dh%dm", h, m)
        }
}

func strVal(m map[string]interface{}, key string) string {
        if v, ok := m[key].(string); ok {
                return v
        }
        return ""
}

// ── Node runner ────────────────────────────────────────────────────────────────

func runNode() error {
        cfg := config.FromEnv()

        zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
        if cfg.LogFormat == "pretty" {
                log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})
        }
        level, err := zerolog.ParseLevel(cfg.LogLevel)
        if err != nil {
                level = zerolog.InfoLevel
        }
        zerolog.SetGlobalLevel(level)

        log.Info().
                Str("version", version).
                Str("mode", cfg.NodeMode).
                Int64("chainId", cfg.ChainID).
                Msg("Starting GYDS boostnode")

        chain := core.NewChain(core.GydsGenesis)
        log.Info().Uint64("height", chain.Height()).Msg("Chain initialised from genesis")

        vs := consensus.NewValidatorSet(core.GydsGenesis.Validators)
        engine := consensus.NewPoSEngine(chain, vs, 5*time.Second)

        rpcSrv := rpc.NewServer(chain, cfg.RPCPort)
        engine.OnNewBlock(func(b *core.Block) {
                log.Info().
                        Uint64("number", b.Header.Number).
                        Str("hash", b.Hash[:16]+"...").
                        Int("txs", len(b.Transactions)).
                        Str("validator", b.Header.Validator).
                        Msg("New block")
                rpcSrv.NotifyNewBlock(b)
        })

        p2pSrv := p2p.NewServer(cfg.P2PPort, cfg.ChainID, chain.Height)
        rpcSrv.SetP2P(p2pSrv)

        for _, addr := range cfg.P2PBootstrap {
                if err := p2pSrv.ConnectTo(addr); err != nil {
                        log.Warn().Err(err).Str("addr", addr).Msg("Failed to connect to bootstrap peer")
                }
        }
        if err := p2pSrv.Start(); err != nil {
                log.Warn().Err(err).Msg("P2P server failed to start (continuing without P2P)")
        }

        engine.Start()
        log.Info().Dur("blockTime", 5*time.Second).Msg("PoS engine started")

        errCh := make(chan error, 1)
        go func() {
                errCh <- rpcSrv.Start()
        }()

        sig := make(chan os.Signal, 1)
        signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

        select {
        case s := <-sig:
                log.Info().Str("signal", s.String()).Msg("Shutting down")
                engine.Stop()
                ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
                defer cancel()
                rpcSrv.Shutdown(ctx)
        case err := <-errCh:
                if err != nil {
                        return fmt.Errorf("RPC server: %w", err)
                }
        }
        return nil
}
