package version

import "fmt"

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

type Info struct {
	Version string
	Commit  string
	Date    string
}

func Get() Info {
	return Info{
		Version: Version,
		Commit:  Commit,
		Date:    Date,
	}
}

func (i Info) String() string {
	return fmt.Sprintf("OpenTalon %s (commit: %s, built: %s)", i.Version, i.Commit, i.Date)
}
