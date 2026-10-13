package smtpd

import "github.com/emersion/go-sasl"

// loginServer implements the obsolete but still common SASL LOGIN mechanism
// (go-sasl only ships a client for it). nodemailer falls back to it when PLAIN
// is not offered.
type loginServer struct {
	authenticate func(username, password string) error
	username     string
	state        int
}

func newLoginServer(authenticate func(username, password string) error) sasl.Server {
	return &loginServer{authenticate: authenticate}
}

func (s *loginServer) Next(response []byte) (challenge []byte, done bool, err error) {
	switch s.state {
	case 0:
		s.state = 1
		if response != nil { // username sent as the initial response
			s.username = string(response)
			s.state = 2
			return []byte("Password:"), false, nil
		}
		return []byte("Username:"), false, nil
	case 1:
		s.username = string(response)
		s.state = 2
		return []byte("Password:"), false, nil
	default:
		pass := string(response)
		s.state = 3
		return nil, true, s.authenticate(s.username, pass)
	}
}
