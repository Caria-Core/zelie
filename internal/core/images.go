package core

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Caria-Core/zelie/internal/build"
	"github.com/Caria-Core/zelie/internal/engine"
)

// An image nothing needs is kept this long, so deleting an app and making
// it again does not download a large image twice.
const keepUnused = 7 * 24 * time.Hour

type sweepRequest struct {
	// Keep lists the images the panel still refers to: those of its apps,
	// databases and the deployments a rollback can go back to.
	Keep []string `json:"keep"`
}

type sweepResponse struct {
	Removed []string `json:"removed"`
	Unused  int      `json:"unused"` // waiting out keepUnused
}

// sweepImages deletes the images that have been unneeded for keepUnused.
// Only the panel knows what its apps need, so it says; the core adds what
// its containers and its builder use.
func (s *Server) sweepImages(w http.ResponseWriter, r *http.Request) {
	var req sweepRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	keep := map[string]bool{}
	for _, ref := range append(req.Keep, build.BuilderImage) {
		name, err := engine.ImageName(ref)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		keep[name] = true
	}
	res, err := s.sweep(r.Context(), keep, time.Now())
	if err != nil {
		s.fail(w, "sweep images", "", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) sweep(ctx context.Context, keep map[string]bool, now time.Time) (sweepResponse, error) {
	res := sweepResponse{Removed: []string{}}
	containers, err := s.Engine.List(ctx)
	if err != nil {
		return res, err
	}
	// A stopped container still needs its image to start again.
	for _, c := range containers {
		keep[c.Image] = true
	}
	images, err := s.Engine.Images(ctx)
	if err != nil {
		return res, err
	}
	var errs []error
	for _, img := range images {
		switch {
		case keep[img.Name]:
			if !img.UnusedSince.IsZero() {
				errs = append(errs, s.Engine.SetUnused(ctx, img.Name, time.Time{}))
			}
		case img.UnusedSince.IsZero():
			errs = append(errs, s.Engine.SetUnused(ctx, img.Name, now))
			res.Unused++
		case now.Sub(img.UnusedSince) < keepUnused:
			res.Unused++
		default:
			if err := s.Engine.RemoveImage(ctx, img.Name); err != nil {
				errs = append(errs, err)
				continue
			}
			s.Log.Info("unused image removed", "image", img.Name, "unused_since", img.UnusedSince)
			res.Removed = append(res.Removed, img.Name)
		}
	}
	return res, errors.Join(errs...)
}

type pinRequest struct {
	Image string `json:"image"`
}

// pinImage names the image a tag points at here by its digest. The panel
// uses it for deployments made before it recorded digests.
func (s *Server) pinImage(w http.ResponseWriter, r *http.Request) {
	var req pinRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	image, err := s.Engine.Pin(r.Context(), req.Image)
	if err != nil {
		s.fail(w, "pin image", "", err)
		return
	}
	writeJSON(w, http.StatusOK, runResponse{Image: image})
}
