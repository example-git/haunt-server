package voice

// Keep these indices stable: the encoded relay uses them on the wire.
var trackPurposes = [...]string{"audio", "camera", "screen", "camera_audio", "screen_audio"}

func audioPurpose(purpose string) bool {
	return purpose == "audio" || purpose == "camera_audio" || purpose == "screen_audio"
}
func blockedSource(publisher, receiver State, purpose string) bool {
	return (audioPurpose(purpose) && receiver.Deafened) ||
		(purpose == "audio" && publisher.Muted) ||
		((purpose == "camera" || purpose == "camera_audio") && publisher.CameraOff) ||
		((purpose == "screen" || purpose == "screen_audio") && publisher.ScreenOff)
}
