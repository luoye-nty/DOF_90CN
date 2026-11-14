package dnfbridge

import (
	"testing"

	dnftown "longheng.io/server/internal/modules/dnf/town"
)

func TestRobotAreaSafePointUsesLargestMovableRectangleCenter(t *testing.T) {
	area := dnftown.Area{
		MovableAreas: []dnftown.MovableArea{
			{X: 10, Y: 20, Width: 20, Height: 10},
			{X: 100, Y: 200, Width: 200, Height: 100},
		},
	}
	x, y := robotAreaSafePoint(area)
	if x != 200 || y != 250 {
		t.Fatalf("safe point = %d,%d want 200,250", x, y)
	}
}

func TestRobotMovementBoundsAroundAcceptsArbitraryCoordinateWithLocalFallback(t *testing.T) {
	service := &Service{}
	bounds, ok := service.RobotMovementBoundsAround(40, 0, 1480, 480)
	if !ok {
		t.Fatal("arbitrary coordinate was rejected")
	}
	if bounds.MinX != 1456 || bounds.MaxX != 1504 || bounds.MinY != 456 || bounds.MaxY != 504 {
		t.Fatalf("fallback bounds = %+v", bounds)
	}
}
