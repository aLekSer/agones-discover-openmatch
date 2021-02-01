package functions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Octops/agones-discover-openmatch/internal/runtime"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/any"
	"github.com/golang/protobuf/ptypes/wrappers"
	"github.com/sirupsen/logrus"
	"open-match.dev/open-match/pkg/pb"
)

const (
	MATCFUNC_NAME = "player_capacity_matchfunc"

	openSlotsKey    = "open-slots"
	playersPerMatch = 20
)

var (
	ErrPlayersCapacityInvalid = errors.New("player capacity must be higher than zero")
)

/*
Criteria for Matches
- Number or tickets should not exceed the PlayerCapacity set by the Status.Players.Capacity field from the GS
*/
func MatchByGamePlayersCapacity(playerCapacity int) MakeMatchesFunc {
	return func(profile *pb.MatchProfile, poolTickets map[string][]*pb.Ticket, poolBackfills map[string][]*pb.Backfill) ([]*pb.Match, error) {
		if err := ValidateMatchFunArguments(playerCapacity, profile, poolTickets); err != nil {
			return nil, err
		}

		logger := runtime.Logger().WithFields(logrus.Fields{
			"component": "match_function",
			"command":   "matchmaker",
		})

		ctx, cancel := context.WithCancel(context.Background())
		chTickets := make(chan *pb.Ticket)

		go func(pool map[string][]*pb.Ticket) {
			defer func() {
				cancel()
			}()

			for _, tickets := range pool {
				for _, ticket := range tickets {
					t := ticket
					chTickets <- t
				}
			}
		}(poolTickets)

		var tickets []*pb.Ticket
		var matches []*pb.Match
		var match *pb.Match

		for {
			select {
			case t := <-chTickets:
				logger.Debugf("creating match for ticket %s", t.Id)
				if tickets == nil || len(match.Tickets) == playerCapacity {
					tickets = []*pb.Ticket{}
					tickets = append(tickets, t)
					id := fmt.Sprintf("profile-%v-%v", profile.GetName(), time.Now().UnixNano())
					matches = append(matches, CreateMatchForTickets(id, profile.GetName(), profile.Extensions, tickets...))
					match = matches[len(matches)-1]
					break
				}

				if len(match.Tickets) < playerCapacity {
					match.Tickets = append(match.Tickets, t)
					break
				}
			case <-ctx.Done():
				logger.Debugf("total matches for profile %s: %d", profile.GetName(), len(matches))
				return matches, nil
			}
		}
	}
}

func MatchWithBackfills() MakeMatchesFunc {
	return func(profile *pb.MatchProfile, poolTickets map[string][]*pb.Ticket, poolBackfills map[string][]*pb.Backfill) ([]*pb.Match, error) {

		//TODO check this: if we have only one profile that should work
		backfills := make([]*pb.Backfill, 0)
		if len(poolBackfills) > 0 {
			for _, v := range poolBackfills {
				backfills = v
				break
			}
		}

		if len(poolTickets) > 0 {
			for _, v := range poolTickets {
				return makeMatches(profile, v, backfills)
			}
		}
		return nil, nil
	}
}

// makeMatches tries to handle backfills at first, then it makes full matches, at the end it makes a match with backfill
// if tickets left
func makeMatches(profile *pb.MatchProfile, tickets []*pb.Ticket, backfills []*pb.Backfill) ([]*pb.Match, error) {
	var matches []*pb.Match
	newMatches, remainingTickets, err := handleBackfills(profile, tickets, backfills)
	if err != nil {
		return nil, err
	}

	matches = append(matches, newMatches...)
	for i := 0; i < len(remainingTickets); i++ {
		match, err := makeMatchWithBackfill(profile, remainingTickets[i:i+1])
		if err != nil {
			return nil, err
		}

		matches = append(matches, match)
	}

	return matches, nil
}

func getOpenSlots(b *pb.Backfill) (int32, error) {
	if b == nil {
		return 0, fmt.Errorf("expected backfill is not nil")
	}

	if b.Extensions != nil {
		if any, ok := b.Extensions[openSlotsKey]; ok {
			var val wrappers.Int32Value
			err := ptypes.UnmarshalAny(any, &val)
			if err != nil {
				return 0, err
			}

			return val.Value, nil
		}
	}

	return playersPerMatch, nil
}

// handleBackfills looks at each backfill's openSlots which is a number of required tickets,
// acquires that tickets, decreases openSlots in backfill and makes a match with updated backfill and associated tickets.
func handleBackfills(profile *pb.MatchProfile, tickets []*pb.Ticket, backfills []*pb.Backfill) ([]*pb.Match, []*pb.Ticket, error) {
	var matches []*pb.Match

	for _, b := range backfills {
		openSlots, err := getOpenSlots(b)
		if err != nil {
			return nil, tickets, err
		}

		var matchTickets []*pb.Ticket
		for openSlots > 0 && len(tickets) > 0 {
			matchTickets = append(matchTickets, tickets[0])
			tickets = tickets[1:]
			openSlots--
		}

		if len(matchTickets) > 0 {
			err := setOpenSlots(b, openSlots)
			if err != nil {
				return nil, tickets, err
			}

			match := newMatch(profile.Name, matchTickets, b)
			matches = append(matches, &match)
		}
	}

	return matches, tickets, nil
}

// makeMatchWithBackfill makes not full match, creates backfill for it with openSlots = playersPerMatch-len(tickets).
func makeMatchWithBackfill(profile *pb.MatchProfile, tickets []*pb.Ticket) (*pb.Match, error) {
	if len(tickets) == 0 {
		return nil, fmt.Errorf("tickets are required")
	}

	if len(tickets) >= playersPerMatch {
		return nil, fmt.Errorf("too many tickets")
	}

	searchFields := tickets[0].SearchFields
	backfill, err := newBackfill(searchFields, playersPerMatch-len(tickets))
	if err != nil {
		return nil, err
	}

	match := newMatch(profile.Name, tickets, backfill)
	// indicates that it is a new match and new game server should be allocated for it
	match.AllocateGameserver = true

	return &match, nil
}

func newBackfill(searchFields *pb.SearchFields, openSlots int) (*pb.Backfill, error) {
	b := pb.Backfill{
		SearchFields: searchFields,
		Generation:   0,
		CreateTime:   ptypes.TimestampNow(),
	}

	err := setOpenSlots(&b, int32(openSlots))
	return &b, err
}

func setOpenSlots(b *pb.Backfill, val int32) error {
	if b.Extensions == nil {
		b.Extensions = make(map[string]*any.Any)
	}

	any, err := ptypes.MarshalAny(&wrappers.Int32Value{Value: val})
	if err != nil {
		return err
	}

	b.Extensions[openSlotsKey] = any
	return nil
}

var matchId = 0

func newMatch(profile string, tickets []*pb.Ticket, b *pb.Backfill) pb.Match {
	matchId++
	t := time.Now().Format("2006-01-02T15:04:05.00")

	return pb.Match{
		MatchId:       fmt.Sprintf("profile-%s-time-%s-num-%d", profile, t, matchId),
		MatchProfile:  profile,
		MatchFunction: "Backfill",
		Tickets:       tickets,
		Backfill:      b,
	}
}

func CreateMatchForTickets(matchID, profileName string, extensions map[string]*any.Any, tickets ...*pb.Ticket) *pb.Match {
	return &pb.Match{
		MatchId:       matchID,
		MatchProfile:  profileName,
		MatchFunction: MATCFUNC_NAME,
		Extensions:    extensions,
		Tickets:       tickets,
	}
}

func ValidateMatchFunArguments(playerCapacity int, profile *pb.MatchProfile, poolTickets map[string][]*pb.Ticket) error {
	if playerCapacity <= 0 {
		return ErrPlayersCapacityInvalid
	}

	if profile == nil {
		return ErrMatchProfileIsNil
	}

	if poolTickets == nil {
		return ErrPoolTicketsIsNil
	}

	return nil
}
