package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

	root.AddCommand(startCmd(), genesisCmd(), versionCmd(), healthCmd())
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
