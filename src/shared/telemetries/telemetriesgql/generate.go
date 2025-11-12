package telemetriesgql

import _ "github.com/suessflorian/gqlfetch"

// Commented out to avoid network timeout issues - schema.graphql should already exist
////go:generate sh -c "go run github.com/suessflorian/gqlfetch/gqlfetch --endpoint https://app.staging.otterize.com/api/telemetry/query > schema.graphql"
//go:generate go run github.com/Khan/genqlient ./genqlient.yaml
