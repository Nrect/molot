// Package app is the use-case catalog of the participant context:
// Application{Commands, Queries} is assembled once in service/NewService
// and injected into every port (BOOK_AUDIT rule 28). Every handler is
// wrapped with the common decorator stack (rule 29).
package app

import (
	"molot/internal/common/decorator"
	"molot/internal/participant/app/command"
	"molot/internal/participant/app/query"
)

type Application struct {
	Commands Commands
	Queries  Queries
}

type Commands struct {
	RegisterParticipant decorator.CommandHandler[command.RegisterParticipant]
	VerifyParticipant   decorator.CommandHandler[command.VerifyParticipant]
}

type Queries struct {
	ParticipantProfile decorator.QueryHandler[query.ParticipantProfile, query.ProfileView]
}
