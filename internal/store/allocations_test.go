package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func ports(list []Allocation) []int {
	var out []int
	for _, a := range list {
		out = append(out, a.Port)
	}
	return out
}

func TestAllocations(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Unix(1_800_000_000, 0)

	if n, err := s.Node(ctx, ThisNode); err != nil || n.Name != "This server" {
		t.Fatalf("Node = %+v, %v", n, err)
	}
	if _, err := s.Node(ctx, 9); !errors.Is(err, ErrNotFound) {
		t.Errorf("a node that is not there: %v", err)
	}
	if list, err := s.Allocations(ctx, ThisNode); err != nil || len(list) != 0 {
		t.Fatalf("the pool starts empty: %v, %v", list, err)
	}

	if err := s.AddAllocations(ctx, ThisNode, AnyAddress, []int{25566, 25565, 25567}, now); err != nil {
		t.Fatal(err)
	}
	list, _ := s.Allocations(ctx, ThisNode)
	if !slices.Equal(ports(list), []int{25565, 25566, 25567}) || list[0].AppID != "" {
		t.Fatalf("allocations %+v", list)
	}

	// A range that runs into the pool adds nothing, not even its free ports.
	var taken *TakenError
	err := s.AddAllocations(ctx, ThisNode, AnyAddress, []int{25568, 25567}, now)
	if !errors.As(err, &taken) || taken.Port != 25567 || !errors.Is(err, ErrExists) {
		t.Fatalf("overlap: %v", err)
	}
	// Every address covers a specific one, and the other way round.
	if err := s.AddAllocations(ctx, ThisNode, "192.0.2.5", []int{25565}, now); !errors.Is(err, ErrExists) {
		t.Errorf("a specific address under 0.0.0.0: %v", err)
	}
	if err := s.AddAllocations(ctx, ThisNode, "192.0.2.5", []int{30000}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.AddAllocations(ctx, ThisNode, AnyAddress, []int{30000}, now); !errors.Is(err, ErrExists) {
		t.Errorf("0.0.0.0 over a specific address: %v", err)
	}
	if err := s.AddAllocations(ctx, ThisNode, "192.0.2.6", []int{30000}, now); err != nil {
		t.Errorf("two specific addresses share a port: %v", err)
	}
	if err := s.AddAllocations(ctx, ThisNode, AnyAddress, []int{40000, 40000}, now); !errors.Is(err, ErrExists) {
		t.Errorf("a port twice: %v", err)
	}
	if list, _ := s.Allocations(ctx, ThisNode); len(list) != 5 {
		t.Errorf("%d allocations after the refused ones", len(list))
	}
}

func TestAssignAllocations(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Unix(1_800_000_000, 0)
	for _, id := range []string{"mc", "other"} {
		if err := s.CreateApp(ctx, App{ID: id, Source: SourceImage, Image: "x", Port: 25565, MemoryMB: 512, CPUs: 1, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddAllocations(ctx, ThisNode, AnyAddress, []int{25565, 25566, 25567}, now); err != nil {
		t.Fatal(err)
	}
	list, _ := s.Allocations(ctx, ThisNode)

	if err := s.AssignAllocations(ctx, "mc", []int64{list[0].ID, list[1].ID}); err != nil {
		t.Fatal(err)
	}
	mine, _ := s.AppAllocations(ctx, "mc")
	if !slices.Equal(ports(mine), []int{25565, 25566}) || mine[0].AppID != "mc" {
		t.Fatalf("mc has %+v", mine)
	}

	// One taken port fails the whole request.
	if err := s.AssignAllocations(ctx, "other", []int64{list[2].ID, list[1].ID}); !errors.Is(err, ErrInUse) {
		t.Fatalf("assigning a used port: %v", err)
	}
	if got, _ := s.AppAllocations(ctx, "other"); len(got) != 0 {
		t.Errorf("a failed request kept %+v", got)
	}
	if err := s.AssignAllocations(ctx, "other", []int64{999}); !errors.Is(err, ErrNotFound) {
		t.Errorf("assigning a port that is not there: %v", err)
	}

	// Only unassigned ports can leave the pool.
	if err := s.DeleteAllocation(ctx, ThisNode, list[0].ID); !errors.Is(err, ErrInUse) {
		t.Errorf("deleting a used port: %v", err)
	}
	if err := s.DeleteAllocation(ctx, ThisNode, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting a port that is not there: %v", err)
	}
	if err := s.DeleteAllocation(ctx, ThisNode+1, list[2].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting through another node: %v", err)
	}
	if err := s.DeleteAllocation(ctx, ThisNode, list[2].ID); err != nil {
		t.Errorf("deleting a free port: %v", err)
	}

	if err := s.UnassignAllocations(ctx, "mc"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.AppAllocations(ctx, "mc"); len(got) != 0 {
		t.Errorf("after unassigning: %+v", got)
	}

	// Deleting a server frees its ports too.
	if err := s.AssignAllocations(ctx, "other", []int64{list[0].ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteApp(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.AppAllocations(ctx, "other"); len(got) != 0 {
		t.Errorf("a deleted app keeps %+v", got)
	}
	if all, _ := s.Allocations(ctx, ThisNode); len(all) != 2 {
		t.Errorf("%d allocations left", len(all))
	}
}

func TestPublicAddress(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if n, _ := s.Node(ctx, ThisNode); n.PublicAddress != "" {
		t.Errorf("starts empty: %q", n.PublicAddress)
	}
	if err := s.SetPublicAddress(ctx, ThisNode, "play.example.com"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Node(ctx, ThisNode); n.PublicAddress != "play.example.com" {
		t.Errorf("address = %q", n.PublicAddress)
	}
	if err := s.SetPublicAddress(ctx, ThisNode, ""); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Node(ctx, ThisNode); n.PublicAddress != "" {
		t.Errorf("not cleared: %q", n.PublicAddress)
	}
	if err := s.SetPublicAddress(ctx, 9, "x.example"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a node that is not there: %v", err)
	}
}
