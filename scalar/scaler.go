package scalar

import (
	"github.com/MagalixCorp/magalix-agent/v2/kuber"
	"github.com/MagalixCorp/magalix-agent/v2/scanner"
	"github.com/MagalixTechnologies/log-go"
	"time"
)

func InitScalars(
	logger *log.Logger,
	scanner *scanner.Scanner,
	kube *kuber.Kube,
	dryRun bool,
) {

	sl := NewScannerListener(logger, scanner)
	oomKilledProcessor := NewOOMKillsProcessor(logger, kube, time.Second, dryRun)

	sl.AddContainerListener(oomKilledProcessor)

	go oomKilledProcessor.Start()
	go sl.Start()
}
