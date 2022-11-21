package itest

import (
	"bufio"
	context "context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/breez/lntest"
	"github.com/breez/lspd/btceclegacy"
	lspd "github.com/breez/lspd/rpc"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/golang/protobuf/proto"
	grpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	lspdExecutable = flag.String(
		"lspdexec", "", "full path to lpsd plugin binary",
	)
	lspdMigrationsDir = flag.String(
		"lspdmigrationsdir", "", "full path to lspd sql migrations directory",
	)
)

var (
	lspBaseFeeMsat uint32 = 1000
	lspFeeRatePpm  uint32 = 1
	lspCltvDelta   uint16 = 40
)

type LspNode struct {
	harness         *lntest.TestHarness
	lightningNode   *lntest.CoreLightningNode
	rpc             lspd.ChannelOpenerClient
	rpcPort         uint32
	rpcHost         string
	privateKey      btcec.PrivateKey
	publicKey       btcec.PublicKey
	postgresBackend *PostgresContainer
	scriptDir       string
}

func NewLspdNode(h *lntest.TestHarness, m *lntest.Miner, name string, timeout time.Time) *LspNode {
	scriptDir := h.GetDirectory(fmt.Sprintf("lspd-%s", name))
	migrationsDir, err := GetMigrationsDir()
	lntest.CheckError(h.T, err)

	pgLogfile := filepath.Join(scriptDir, "postgres.log")
	postgresBackend := StartPostgresContainer(h.T, h.Ctx, pgLogfile)
	postgresBackend.RunMigrations(h.T, h.Ctx, migrationsDir)

	lspdBinary, err := GetLspdBinary()
	lntest.CheckError(h.T, err)

	lspdPort, err := lntest.GetPort()
	lntest.CheckError(h.T, err)

	lspdPrivateKeyBytes, err := GenerateRandomBytes(32)
	lntest.CheckError(h.T, err)

	priv, publ := btcec.PrivKeyFromBytes(lspdPrivateKeyBytes)

	host := "localhost"
	grpcAddress := fmt.Sprintf("%s:%d", host, lspdPort)
	env := []string{
		"NODE_NAME=lsp",
		"NODE_PUBKEY=dunno",
		"NODE_HOST=host:port",
		"RUN_CLN=true",
		"TOKEN=hello",
		fmt.Sprintf("DATABASE_URL=%s", postgresBackend.ConnectionString()),
		fmt.Sprintf("LISTEN_ADDRESS=%s", grpcAddress),
		fmt.Sprintf("LSPD_PRIVATE_KEY=%x", lspdPrivateKeyBytes),
	}

	scriptFilePath := filepath.Join(scriptDir, "start-lspd.sh")
	log.Printf("Creating lspd startup script at %s", scriptFilePath)
	scriptFile, err := os.OpenFile(scriptFilePath, os.O_CREATE|os.O_WRONLY, 0755)
	lntest.CheckError(h.T, err)

	writer := bufio.NewWriter(scriptFile)
	_, err = writer.WriteString("#!/bin/bash\n")
	lntest.CheckError(h.T, err)

	for _, str := range env {
		_, err = writer.WriteString("export " + str + "\n")
		lntest.CheckError(h.T, err)
	}

	_, err = writer.WriteString(lspdBinary + "\n")
	lntest.CheckError(h.T, err)

	err = writer.Flush()
	lntest.CheckError(h.T, err)
	scriptFile.Close()

	args := []string{
		fmt.Sprintf("--plugin=%s", scriptFilePath),
		fmt.Sprintf("--fee-base=%d", lspBaseFeeMsat),
		fmt.Sprintf("--fee-per-satoshi=%d", lspFeeRatePpm),
		fmt.Sprintf("--cltv-delta=%d", lspCltvDelta),
	}

	lightningNode := lntest.NewCoreLightningNode(h, m, name, timeout, args...)

	conn, err := grpc.Dial(
		grpcAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(&token{token: "hello"}),
	)
	lntest.CheckError(h.T, err)

	client := lspd.NewChannelOpenerClient(conn)

	lspNode := &LspNode{
		harness:         h,
		lightningNode:   lightningNode,
		rpc:             client,
		rpcPort:         lspdPort,
		rpcHost:         host,
		privateKey:      *priv,
		publicKey:       *publ,
		postgresBackend: postgresBackend,
		scriptDir:       scriptDir,
	}

	h.AddStoppable(lspNode)
	h.AddCleanable(lspNode)
	h.RegisterLogfile(pgLogfile, fmt.Sprintf("%s-postgres", name))
	return lspNode
}

func (l *LspNode) RegisterPayment(paymentInfo *lspd.PaymentInformation) {
	serialized, err := proto.Marshal(paymentInfo)
	lntest.CheckError(l.harness.T, err)

	encrypted, err := btceclegacy.Encrypt(&l.publicKey, serialized)
	lntest.CheckError(l.harness.T, err)

	log.Printf("Registering payment")
	_, err = l.rpc.RegisterPayment(
		l.harness.Ctx,
		&lspd.RegisterPaymentRequest{
			Blob: encrypted,
		},
	)
	lntest.CheckError(l.harness.T, err)
}

func (l *LspNode) TearDown() error {
	// NOTE: The lightningnode will be torn down on its own.
	return l.postgresBackend.Shutdown(l.harness.Ctx)
}

func (l *LspNode) Cleanup() error {
	return l.postgresBackend.Cleanup(l.harness.Ctx)
}

func (l *LspNode) NodeId() []byte {
	return l.lightningNode.NodeId()
}

func GetLspdBinary() (string, error) {
	if lspdExecutable != nil {
		return *lspdExecutable, nil
	}

	return exec.LookPath("lspd")
}

func GetMigrationsDir() (string, error) {
	if lspdMigrationsDir != nil {
		return *lspdMigrationsDir, nil
	}

	return exec.LookPath("lspdmigrationsdir")
}

type token struct {
	token string
}

func (t *token) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	m := make(map[string]string)
	m["authorization"] = "Bearer " + t.token
	return m, nil
}

// RequireTransportSecurity indicates whether the credentials requires
// transport security.
func (t *token) RequireTransportSecurity() bool {
	return false
}
