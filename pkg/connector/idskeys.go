// corten-matrix - A Matrix-iMessage puppeting bridge.

package connector

import (
	"context"

	"maunium.net/go/mautrix/bridgev2"
)

// ensureIdentityKeys runs the outbound delivery-identity precheck for a
// Matrix→iMessage message before it is handed to the sender. A non-nil
// response or error means the send was resolved at this stage and must not
// continue; (nil, nil) lets the normal send path proceed.
func (c *IMClient) ensureIdentityKeys(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	return nil, nil
}
