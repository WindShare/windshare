// Package link constructs and parses sender-authenticated capability URLs:
//
//	https://<frontend>/<shareId>[?r=<relay-base>]#<base64url(suite||readSecret||pkHash)>
//
// An omitted relay hint selects the link's origin. A single equivalent relay at
// that origin's root is omitted when constructing links; explicit hints preserve
// their order. The fragment never travels in HTTP requests. Split and Merge
// support delivering the link and key separately. All operations are pure.
package link
