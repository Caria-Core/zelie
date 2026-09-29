package steam

import "strconv"

// Image is the container that asks Steam for the latest builds: Valve's
// SteamCMD with nothing else in it, pinned so a moved tag changes nothing.
const Image = "steamcmd/steamcmd:ubuntu-24@sha256:48f967bb753b38bab0049318888acf33a8e227fbc41d7fa308fc37d4e7595958"

// InfoCommand is the command line that prints the store information of
// the apps, logged in anonymously. app_info_update makes SteamCMD fetch it
// rather than print what it cached.
func InfoCommand(appIDs []int64) []string {
	args := []string{"steamcmd", "+login", "anonymous", "+app_info_update", "1"}
	for _, id := range appIDs {
		args = append(args, "+app_info_print", strconv.FormatInt(id, 10))
	}
	return append(args, "+quit")
}
