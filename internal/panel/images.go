package panel

import (
	"context"
	"strings"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
)

const sweepEvery = 24 * time.Hour

// runImageSweep tells the core, once a day, which images the apps still
// need. The core deletes the ones nothing has needed for a week.
func (s *Server) runImageSweep(ctx context.Context) {
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		s.sweepImages(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) sweepImages(ctx context.Context) {
	keep, err := s.Store.Images(ctx)
	if err != nil {
		s.Log.Error("images: list", "err", err)
		return
	}
	removed, err := s.Core.SweepImages(ctx, append(keep, s.steamImages(ctx)...))
	if err != nil {
		s.Log.Error("images: sweep", "err", err)
	}
	if len(removed) > 0 {
		s.Log.Info("unused images removed", "images", removed)
	}
}

// pinLive records the digest of what the live deployments run, for those
// made before Zelie kept it. Without it a restart would pull the tag again
// and could quietly start a newer build.
func (s *Server) pinLive(ctx context.Context) {
	live, err := s.Store.LiveDeployments(ctx)
	if err != nil {
		s.Log.Error("images: live deployments", "err", err)
		return
	}
	for _, d := range live {
		if d.Image == "" || strings.HasPrefix(d.Image, engine.LocalImages) || engine.Pinned(d.Image) {
			continue
		}
		pinned, err := s.Core.Pin(ctx, d.Image)
		if err != nil {
			s.Log.Error("images: pin", "app", d.AppID, "image", d.Image, "err", err)
			continue
		}
		if err := s.Store.SetDeploymentImage(ctx, d.ID, pinned); err != nil {
			s.Log.Error("images: pin", "app", d.AppID, "err", err)
		}
	}
}

// removeAppImages deletes what building a deleted app left behind. The
// images it pulled go with the daily sweep, in case it is made again.
func (s *Server) removeAppImages(ctx context.Context, app string) {
	list, err := s.Store.Unpruned(ctx, app)
	if err != nil {
		s.Log.Error("list images", "app", app, "err", err)
	}
	for _, d := range list {
		if !strings.HasPrefix(d.Image, engine.LocalImages) {
			continue
		}
		if err := s.Core.RemoveImage(ctx, d.Image); err != nil {
			s.Log.Error("remove image", "image", d.Image, "err", err)
		}
	}
	if err := s.Core.RemoveBuildCache(ctx, app); err != nil {
		s.Log.Error("remove build cache", "app", app, "err", err)
	}
}
