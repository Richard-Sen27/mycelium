package bundle

// DistroRequirements returns, for every program this package runs on the
// sandbox host besides the image's own tools, the argv that checks it is
// there: runsc, iproute2, nft (the firewall tried first), setsid with
// --wait (util-linux), coreutils (du -B1), GNU tar (--numeric-owner) and
// findutils. `mycel sandbox setup` installs and verifies exactly these
// inside the mycel-sbx WSL distro.
func DistroRequirements() [][]string {
	return [][]string{
		Runsc{}.Version(),
		ToolVersionArgv("ip"),
		ToolVersionArgv("nft"),
		ToolVersionArgv("setsid"),
		ToolVersionArgv("du"),
		ToolVersionArgv("tar"),
		ToolVersionArgv("find"),
	}
}
