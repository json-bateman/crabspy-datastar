package web

type LoginSignals struct {
	Name   string `json:"name"`
	Secret string `json:"secret"`
}

type HostSignals struct {
	Name          string `json:"roomName"`
	Locations     string `json:"locations"`
	MaxPlayers    string `json:"maxPlayers"`
	TimerDuration string `json:"timerDuration"`
}

type HostRules struct {
	NameTooLong bool
	NameEmpty   bool
}

type Player struct {
	Username    string
	DisplayName string
	CrabAvatar  string
}
