package credits

type PaceClass string

const (
	ClassExhausted  PaceClass = "exhausted"
	ClassOverpace   PaceClass = "overpace"
	ClassOnpace     PaceClass = "onpace"
	ClassUnderspent PaceClass = "underspent"
)

func PaceClassOf(used, elapsed, floorPct int) PaceClass {
	if used >= 95 {
		return ClassExhausted
	}
	floor := 4
	if elapsed < floor {
		elapsed = floor
	}
	if elapsed == 0 {
		elapsed = 4
	}
	pace := used * 100 / elapsed
	if pace > 150 && used >= floorPct {
		return ClassOverpace
	}
	if pace < 60 {
		return ClassUnderspent
	}
	return ClassOnpace
}

func ClassConcurrency(c PaceClass) int {
	switch c {
	case ClassExhausted:
		return 0
	case ClassOverpace:
		return 1
	case ClassOnpace:
		return 2
	case ClassUnderspent:
		return 3
	default:
		return 2
	}
}
