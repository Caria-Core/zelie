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
	CNIVersion        = "1.9.1"
	RailpackVersion   = "0.40.0"
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

var cniArtifacts = map[string]artifact{
	"amd64": {
		URL:    "https://github.com/containernetworking/plugins/releases/download/v1.9.1/cni-plugins-linux-amd64-v1.9.1.tgz",
		SHA256: "b98f74a0f8522f0a83867178729c1aa70f2158f90c45a2ca8fa791db1c76b303",
	},
	"arm64": {
		URL:    "https://github.com/containernetworking/plugins/releases/download/v1.9.1/cni-plugins-linux-arm64-v1.9.1.tgz",
		SHA256: "56171987d3947707c3563db2f4001bccaf50fd63468611b9f3cbecb1375ee7ec",
	},
}

// Railpack works out how to build a repository that has no Dockerfile. It
// runs inside the build containers, never on the host itself.
var railpackArtifacts = map[string]artifact{
	"amd64": {
		URL:    "https://github.com/railwayapp/railpack/releases/download/v0.40.0/railpack-v0.40.0-x86_64-unknown-linux-musl.tar.gz",
		SHA256: "52e5558f830b7349386e9448e04351d0834df041d4c31c20b7ec4d539f0d66d7",
	},
	"arm64": {
		URL:    "https://github.com/railwayapp/railpack/releases/download/v0.40.0/railpack-v0.40.0-arm64-unknown-linux-musl.tar.gz",
		SHA256: "86c5a3db888c710b2014262de55fc87a56c85c4e96ba7cd10894e5df210a212b",
	},
}

// cniPlugins are the network plugins Zelie uses. The archive ships many more.
var cniPlugins = []string{"bridge", "host-local", "loopback", "firewall", "portmap"}

// containerdBinaries are the files we take from the containerd archive. The
// archive has more, but Zelie only needs the daemon, the runc shim, and ctr
// for debugging by hand.
var containerdBinaries = []string{"containerd", "containerd-shim-runc-v2", "ctr"}
