package app

type AccountProfile struct {
	Account         Account
	FollowerCount   int64
	FollowingCount  int64
	PostCount       int64
	ViewerFollowing *bool
}
