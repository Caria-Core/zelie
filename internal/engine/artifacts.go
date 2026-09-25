package engine

// Zelie runs its own copy of containerd and runc instead of whatever the
// distribution ships, so every server runs the versions we tested. The
// checksums are pinned here rather than fetched next to the download, which
// means a compromised release page cannot swap the binaries.
//
// To upgrade, change the versions and checksums together. The checksums come
// from the .sha256sum files published with each release.

const (
	ContainerdVersion = "2.4.1"
	RuncVersion       = "1.5.1"
)

type artifact struct {
	URL    string
	SHA256 string
}

var containerdArtifacts = map[string]artifact{
	"amd64": {
		URL:    "https://github.com/containerd/containerd/releases/download/v2.4.1/containerd-static-2.4.1-linux-amd64.tar.gz",
		SHA256: "02ad3a7e80d7d2c018c7134d5eca7e301283db6895f100b957fb52ad8e5a30fd",
	},
	"arm64": {
		URL:    "https://github.com/containerd/containerd/releases/download/v2.4.1/containerd-static-2.4.1-linux-arm64.tar.gz",
		SHA256: "d3aba9347650505df79858bb9cbf1be52080fd8145f72dd9308d4936f487fb74",
	},
}

var runcArtifacts = map[string]artifact{
	"amd64": {
		URL:    "https://github.com/opencontainers/runc/releases/download/v1.5.1/runc.amd64",
		SHA256: "177df879d50c913eb205e898d5c1c05a18f574053c0ce5524c471208eaf06f6f",
	},
	"arm64": {
		URL:    "https://github.com/opencontainers/runc/releases/download/v1.5.1/runc.arm64",
		SHA256: "ca70e7dbd6616ca782a59b5d3ac86909123fdaa9fa3f89dcf29051c70eee7ce9",
	},
}

// containerdBinaries are the files we take from the containerd archive. The
// archive has more, but Zelie only needs the daemon, the runc shim, and ctr
// for debugging by hand.
var containerdBinaries = []string{"containerd", "containerd-shim-runc-v2", "ctr"}
