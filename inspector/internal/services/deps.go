package services

import (
	"log/slog"

	"github.com/dockergiant/images/inspector/internal/capture"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/magento"
	"github.com/dockergiant/images/inspector/internal/procmap"
)

type Deps struct {
	Docker  docker.API
	Magento magento.Reader
	Proc    *procmap.Map
	Capture *capture.Manager
	Log     *slog.Logger
}
