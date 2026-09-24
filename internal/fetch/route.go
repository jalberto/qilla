package fetch

import "github.com/jalberto/qilla/internal/decide"

// RouteQuestion is the fetch-route decider's question.
const RouteQuestion = "Which fetch method is most likely to return the article text for this URL on the first try?"

func askRouteRequest(text string) decide.AskRequest {
	return decide.AskRequest{
		Kind:     "choice",
		Options:  append([]string{}, DefaultOrder...),
		Question: RouteQuestion,
		Text:     text,
		Caller:   "fetch-route",
		// A URL plus its HEAD headers is public: auto → Jev, kev fallback.
		Public: true,
		Route:  "auto",
	}
}
