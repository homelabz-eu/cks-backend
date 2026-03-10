package services

import (
	"context"
	"fmt"

	"github.com/homelabz-eu/cks-backend/internal/config"
	"github.com/homelabz-eu/cks-backend/internal/kubevirt"
)

type TerminalServiceImpl struct {
	kubevirtClient  *kubevirt.Client
	terminalMgmtURL string
}

func NewTerminalService(kubevirtClient *kubevirt.Client, cfg *config.Config) TerminalService {
	return &TerminalServiceImpl{
		kubevirtClient:  kubevirtClient,
		terminalMgmtURL: cfg.TerminalMgmtURL,
	}
}

func (t *TerminalServiceImpl) GetTerminalURL(ctx context.Context, namespace, vmName string) (string, error) {
	vmIP := t.kubevirtClient.GetVMIP(ctx, namespace, vmName)
	if vmIP == "" || vmIP == "0.0.0.0" {
		return "", fmt.Errorf("failed to resolve IP for VM %s in namespace %s", vmName, namespace)
	}
	return fmt.Sprintf("%s/terminal?vmIP=%s", t.terminalMgmtURL, vmIP), nil
}
