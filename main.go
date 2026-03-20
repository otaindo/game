package main

import (
	"encoding/json"
	"fmt"
	"image/color"
	"log"
	"math"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/ebitenutil"
	"github.com/hajimehoshi/ebiten/v2/inpututil"
)

const (
	screenWidth  = 1920
	screenHeight = 1080
	gravity      = 0.35
	wormRadius   = 22.0
	blastRadius  = 140.0
	maxPower     = 180.0

	particleLifetime = 60
)

var (
	upgrader  = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	clients   = make(map[*websocket.Conn]bool)
	broadcast = make(chan []byte)
	mutex     sync.Mutex
)

type NetMessage struct {
	Type    string
	Name    string
	X, Y    float64
	Angle   float64
	Power   float64
	Content string

	Players map[string]*Worm
	Terrain []int
}

type Worm struct {
	X, Y       float64
	TargetX    float64
	TargetY    float64
	VX, VY     float64
	HP         int
	Color      color.RGBA
	Angle      float64
	Power      float64
}

type Projectile struct {
	X, Y, VX, VY float64
	Active       bool
}

type Terrain struct {
	Heights []int
}

type VisualData struct {
	HitFlash float64
	BobPhase float64
	LastX    float64
	LastY    float64
}

type Particle struct {
	X, Y   float64
	VX, VY float64
	Life   int
	Color  color.RGBA
	Size   float64
}

type Game struct {
	conn       *websocket.Conn
	mu         sync.Mutex
	MyName     string
	IsHost     bool
	Players    map[string]*Worm
	Projectile *Projectile
	Terrain    *Terrain

	ChatLog  []string
	InputMsg string
	IsTyping bool

	visuals       map[string]*VisualData
	particles     []Particle
	groundTexture *ebiten.Image
	skyGradient   *ebiten.Image
}

func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func randomColor() color.RGBA {
	return color.RGBA{
		uint8(rand.Intn(200)),
		uint8(rand.Intn(200)),
		255,
		255,
	}
}

func createGroundTexture() *ebiten.Image {
	const texW, texH = 64, 64
	img := ebiten.NewImage(texW, texH)
	for y := 0; y < texH; y++ {
		for x := 0; x < texW; x++ {
			v := rand.Intn(50) + 100
			col := color.RGBA{uint8(v), uint8(v/2 + 30), uint8(v/4), 255}
			img.Set(x, y, col)
		}
	}
	return img
}

func createSkyGradient() *ebiten.Image {
	img := ebiten.NewImage(screenWidth, screenHeight)
	for y := 0; y < screenHeight; y++ {
		t := float64(y) / screenHeight
		r := uint8(135 * (1 - t))
		g := uint8(206 * (1 - t))
		b := uint8(235 * (1 - t))
		for x := 0; x < screenWidth; x++ {
			img.Set(x, y, color.RGBA{r, g, b, 255})
		}
	}
	return img
}

func (g *Game) addExplosion(x, y float64) {
	for i := 0; i < 40; i++ {
		angle := rand.Float64() * 2 * math.Pi
		speed := rand.Float64()*8 + 4
		vx := math.Cos(angle) * speed
		vy := math.Sin(angle) * speed
		life := rand.Intn(particleLifetime) + 30
		g.particles = append(g.particles, Particle{
			X:     x,
			Y:     y,
			VX:    vx,
			VY:    vy,
			Life:  life,
			Color: color.RGBA{255, 100 + uint8(rand.Intn(100)), 0, 255},
			Size:  3 + rand.Float64()*4,
		})
	}
}

func (g *Game) updateParticles() {
	newParticles := []Particle{}
	for _, p := range g.particles {
		p.X += p.VX
		p.Y += p.VY
		p.VY += gravity * 0.5
		p.Life--
		if p.Life > 0 && p.X >= 0 && p.X <= screenWidth && p.Y <= screenHeight {
			newParticles = append(newParticles, p)
		}
	}
	g.particles = newParticles
}

func (g *Game) updateVisuals() {
	for name, w := range g.Players {
		v, ok := g.visuals[name]
		if !ok {
			g.visuals[name] = &VisualData{
				BobPhase: rand.Float64() * 2 * math.Pi,
				LastX:    w.X,
				LastY:    w.Y,
			}
			v = g.visuals[name]
		}
		if v.HitFlash > 0 {
			v.HitFlash -= 0.05
			if v.HitFlash < 0 {
				v.HitFlash = 0
			}
		}
		dx := w.X - v.LastX
		dy := w.Y - v.LastY
		if math.Abs(dx)+math.Abs(dy) > 0.5 {
			v.BobPhase += 0.2
		} else {
			v.BobPhase += 0.02
		}
		v.LastX, v.LastY = w.X, w.Y
	}
}

// Улучшенная отрисовка земли: сначала сплошной цвет, затем текстура
func (g *Game) drawTerrain(screen *ebiten.Image) {
	if g.Terrain == nil {
		return
	}
	baseColor := color.RGBA{70, 40, 20, 255} // тёмно-коричневый

	for x := 0; x < len(g.Terrain.Heights); x++ {
		groundY := g.Terrain.Heights[x]
		if groundY >= screenHeight {
			continue
		}
		height := screenHeight - groundY
		if height <= 0 {
			continue
		}
		// Заливка цветом
		ebitenutil.DrawRect(screen, float64(x), float64(groundY), 1, float64(height), baseColor)

		// Текстура поверх (прозрачная или наложение)
		op := &ebiten.DrawImageOptions{}
		op.GeoM.Translate(float64(x), float64(groundY))
		op.GeoM.Scale(1, float64(height)/64)
		// Делаем текстуру полупрозрачной, чтобы был виден цвет
		op.ColorM.Scale(1, 1, 1, 0.6)
		screen.DrawImage(g.groundTexture, op)
	}
}

func (g *Game) drawWorm(screen *ebiten.Image, name string, w *Worm) {
	if w.HP <= 0 {
		return
	}
	v := g.visuals[name]
	if v == nil {
		v = &VisualData{}
	}

	bob := math.Sin(v.BobPhase) * 2.0

	col := w.Color
	if v.HitFlash > 0 {
		col = color.RGBA{255, uint8(float64(w.Color.G) * (1 - v.HitFlash)), uint8(float64(w.Color.B) * (1 - v.HitFlash)), 255}
	}

	shadowColor := color.RGBA{0, 0, 0, 80}
	ebitenutil.DrawCircle(screen, w.X+3, w.Y+3+bob, wormRadius, shadowColor)

	ebitenutil.DrawCircle(screen, w.X, w.Y+bob, wormRadius, col)

	eyeXOffset := wormRadius * 0.5
	eyeYOffset := wormRadius * 0.3
	eyeSize := wormRadius * 0.25
	ebitenutil.DrawCircle(screen, w.X-eyeXOffset, w.Y+eyeYOffset+bob, eyeSize, color.White)
	ebitenutil.DrawCircle(screen, w.X+eyeXOffset, w.Y+eyeYOffset+bob, eyeSize, color.White)

	angle := w.Angle
	if w.Power > 0 {
		angle = w.Angle
	}
	dx := math.Cos(angle) * (eyeSize * 0.6)
	dy := math.Sin(angle) * (eyeSize * 0.6)
	ebitenutil.DrawCircle(screen, w.X-eyeXOffset+dx, w.Y+eyeYOffset+bob+dy, eyeSize*0.5, color.Black)
	ebitenutil.DrawCircle(screen, w.X+eyeXOffset+dx, w.Y+eyeYOffset+bob+dy, eyeSize*0.5, color.Black)

	smileAngle := math.Pi / 3
	for t := -smileAngle; t <= smileAngle; t += 0.1 {
		x := w.X + math.Cos(t)*wormRadius*0.6
		y := w.Y + math.Sin(t)*wormRadius*0.2 + bob + wormRadius*0.2
		screen.Set(int(x), int(y), color.Black)
	}

	hpWidth := wormRadius * 2
	hpHeight := 6.0
	hpX := w.X - hpWidth/2
	hpY := w.Y - wormRadius - 10 + bob
	ebitenutil.DrawRect(screen, hpX, hpY, hpWidth, hpHeight, color.RGBA{50, 50, 50, 200})
	healthPercent := float64(w.HP) / 100.0
	if healthPercent > 0 {
		fillWidth := hpWidth * healthPercent
		var hpColor color.Color
		if healthPercent > 0.5 {
			hpColor = color.RGBA{0, 200, 0, 255}
		} else if healthPercent > 0.2 {
			hpColor = color.RGBA{200, 200, 0, 255}
		} else {
			hpColor = color.RGBA{200, 0, 0, 255}
		}
		ebitenutil.DrawRect(screen, hpX, hpY, fillWidth, hpHeight, hpColor)
	}
}

func (g *Game) drawAiming(screen *ebiten.Image, w *Worm) {
	if w.Power <= 0 {
		return
	}
	mx, my := ebiten.CursorPosition()
	endX, endY := float64(mx), float64(my)
	dx, dy := endX-w.X, endY-w.Y
	length := math.Hypot(dx, dy)
	if length > 0 {
		steps := int(length / 8)
		for i := 1; i <= steps; i++ {
			t := float64(i) / float64(steps)
			px := w.X + dx*t
			py := w.Y + dy*t
			if i%2 == 0 {
				screen.Set(int(px), int(py), color.RGBA{255, 255, 255, 200})
			}
		}
	}

	powerAngle := w.Angle
	powerLen := w.Power / maxPower * 50
	arrowX := w.X + math.Cos(powerAngle)*powerLen
	arrowY := w.Y + math.Sin(powerAngle)*powerLen
	ebitenutil.DrawLine(screen, w.X, w.Y, arrowX, arrowY, color.RGBA{255, 100, 0, 200})
	ebitenutil.DrawCircle(screen, arrowX, arrowY, 5, color.RGBA{255, 200, 0, 200})
}

func (g *Game) drawProjectile(screen *ebiten.Image) {
	if g.Projectile == nil {
		return
	}
	p := g.Projectile
	ebitenutil.DrawCircle(screen, p.X, p.Y, 6, color.RGBA{0, 0, 0, 255})
	ebitenutil.DrawCircle(screen, p.X-2, p.Y-2, 3, color.RGBA{255, 100, 0, 200})
}

func (g *Game) drawChat(screen *ebiten.Image) {
	const chatWidth, chatHeight = 420, 160
	ebitenutil.DrawRect(screen, 12, 12, chatWidth, chatHeight, color.RGBA{0, 0, 0, 80})
	ebitenutil.DrawRect(screen, 10, 10, chatWidth, chatHeight, color.RGBA{30, 30, 30, 220})

	y := 20
	for i := len(g.ChatLog) - 1; i >= 0 && i > len(g.ChatLog)-6; i-- {
		ebitenutil.DebugPrintAt(screen, g.ChatLog[i], 20, y)
		y += 20
	}

	if g.IsTyping {
		inputBg := color.RGBA{0, 0, 0, 180}
		ebitenutil.DrawRect(screen, 10, screenHeight-40, 400, 30, inputBg)
		ebitenutil.DebugPrintAt(screen, "> "+g.InputMsg, 20, screenHeight-30)
	}
}

func (g *Game) drawPlayerInfo(screen *ebiten.Image) {
	me := g.Players[g.MyName]
	if me == nil {
		return
	}
	infoX := screenWidth - 200
	infoY := 20
	ebitenutil.DrawRect(screen, float64(infoX), float64(infoY), 180, 80, color.RGBA{0, 0, 0, 180})
	ebitenutil.DebugPrintAt(screen, "Player: "+g.MyName, infoX+10, infoY+10)
	ebitenutil.DebugPrintAt(screen, fmt.Sprintf("HP: %d", me.HP), infoX+10, infoY+30)
	ebitenutil.DebugPrintAt(screen, fmt.Sprintf("Power: %.0f", me.Power), infoX+10, infoY+50)
}

func (t *Terrain) Dig(x, r int) {
	for i := x - r; i < x+r; i++ {
		if i >= 0 && i < len(t.Heights) {
			dist := math.Abs(float64(i - x))
			depth := int(math.Max(0, math.Sqrt(float64(r*r)-dist*dist)))
			t.Heights[i] += depth
		}
	}
}

func (g *Game) BroadcastWorld() {
	if !g.IsHost {
		return
	}
	msg := NetMessage{
		Type:    "world",
		Players: g.Players,
		Terrain: g.Terrain.Heights,
	}
	g.Send(msg)
}

func (g *Game) Update() error {
	if g.Terrain == nil {
		return nil
	}

	if inpututil.IsKeyJustPressed(ebiten.KeyT) && !g.IsTyping {
		g.IsTyping = true
		return nil
	}
	if g.IsTyping {
		if inpututil.IsKeyJustPressed(ebiten.KeyEnter) {
			if g.InputMsg != "" {
				g.Send(NetMessage{Type: "chat", Name: g.MyName, Content: g.InputMsg})
				g.InputMsg = ""
			}
			g.IsTyping = false
		}
		g.InputMsg += string(ebiten.AppendInputChars(nil))
		if inpututil.IsKeyJustPressed(ebiten.KeyBackspace) && len(g.InputMsg) > 0 {
			g.InputMsg = g.InputMsg[:len(g.InputMsg)-1]
		}
		return nil
	}

	me := g.Players[g.MyName]
	if me != nil {
		if ebiten.IsKeyPressed(ebiten.KeyA) {
			me.VX = -3
		} else if ebiten.IsKeyPressed(ebiten.KeyD) {
			me.VX = 3
		} else {
			me.VX = 0
		}
		if inpututil.IsKeyJustPressed(ebiten.KeySpace) && me.VY == 0 {
			me.VY = -10
		}

		mx, my := ebiten.CursorPosition()
		me.Angle = math.Atan2(float64(my)-me.Y, float64(mx)-me.X)

		if ebiten.IsMouseButtonPressed(ebiten.MouseButtonLeft) {
			me.Power += 2
			if me.Power > maxPower {
				me.Power = maxPower
			}
		} else if me.Power > 0 {
			g.Send(NetMessage{
				Type:  "shoot",
				Name:  g.MyName,
				X:     me.X,
				Y:     me.Y,
				Angle: me.Angle,
				Power: me.Power,
			})
			me.Power = 0
		}

		me.VY += gravity
		me.Y += me.VY
		me.X += me.VX
		ix := int(clamp(me.X, 0, screenWidth-1))
		groundY := float64(g.Terrain.Heights[ix]) - wormRadius
		if me.Y > groundY {
			me.Y = groundY
			me.VY = 0
		}

		g.Send(NetMessage{
			Type:  "input",
			Name:  g.MyName,
			X:     me.X,
			Y:     me.Y,
			Angle: me.Angle,
			Power: me.Power,
		})
	}

	if g.IsHost && g.Projectile != nil {
		p := g.Projectile
		p.VY += gravity
		p.X += p.VX
		p.Y += p.VY

		ix := int(clamp(p.X, 0, screenWidth-1))
		if p.Y > float64(g.Terrain.Heights[ix]) {
			g.Terrain.Dig(ix, 70)
			g.addExplosion(p.X, p.Y)

			for name, w := range g.Players {
				dist := math.Hypot(w.X-p.X, w.Y-p.Y)
				if dist < blastRadius {
					damage := int(80 * (1 - dist/blastRadius))
					w.HP -= damage
					if v, ok := g.visuals[name]; ok {
						v.HitFlash = 1.0
					}
				}
			}
			g.Projectile = nil
		}
	}

	for _, p := range g.Players {
		p.X += (p.TargetX - p.X) * 0.2
		p.Y += (p.TargetY - p.Y) * 0.2
	}

	g.updateParticles()
	g.updateVisuals()

	if g.IsHost {
		g.BroadcastWorld()
	}

	return nil
}

func (g *Game) Draw(screen *ebiten.Image) {
	if g.Terrain == nil {
		ebitenutil.DebugPrint(screen, "WAITING FOR HOST...")
		return
	}

	screen.DrawImage(g.skyGradient, nil)
	g.drawTerrain(screen)
	g.drawProjectile(screen)

	for name, w := range g.Players {
		if w.HP <= 0 {
			continue
		}
		g.drawWorm(screen, name, w)
	}

	me := g.Players[g.MyName]
	if me != nil && me.HP > 0 && !g.IsTyping {
		g.drawAiming(screen, me)
	}

	for _, p := range g.particles {
		ebitenutil.DrawCircle(screen, p.X, p.Y, p.Size, p.Color)
	}

	g.drawChat(screen)
	g.drawPlayerInfo(screen)
}

func (g *Game) Send(m NetMessage) {
	data, _ := json.Marshal(m)
	_ = g.conn.WriteMessage(websocket.TextMessage, data)
}

func (g *Game) ReceiveLoop() {
	for {
		_, msg, err := g.conn.ReadMessage()
		if err != nil {
			return
		}
		var m NetMessage
		_ = json.Unmarshal(msg, &m)

		g.mu.Lock()

		switch m.Type {
		case "input":
			if g.IsHost {
				p := g.Players[m.Name]
				if p == nil {
					g.Players[m.Name] = &Worm{HP: 100, Color: randomColor()}
					p = g.Players[m.Name]
				}
				p.TargetX = m.X
				p.TargetY = m.Y
				p.Angle = m.Angle
			}
		case "world":
			if !g.IsHost {
				g.Players = m.Players
				g.Terrain = &Terrain{Heights: m.Terrain}
			}
		case "shoot":
			if g.IsHost {
				g.Projectile = &Projectile{
					X:  m.X,
					Y:  m.Y,
					VX: math.Cos(m.Angle) * m.Power * 0.2,
					VY: math.Sin(m.Angle) * m.Power * 0.2,
				}
			}
		case "chat":
			g.ChatLog = append(g.ChatLog, m.Name+": "+m.Content)
			if len(g.ChatLog) > 50 {
				g.ChatLog = g.ChatLog[1:]
			}
		}

		g.mu.Unlock()
	}
}

func startServer() {
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		ws, _ := upgrader.Upgrade(w, r, nil)
		mutex.Lock()
		clients[ws] = true
		mutex.Unlock()

		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				mutex.Lock()
				delete(clients, ws)
				mutex.Unlock()
				break
			}
			broadcast <- msg
		}
	})

	go func() {
		for msg := range broadcast {
			mutex.Lock()
			for c := range clients {
				_ = c.WriteMessage(websocket.TextMessage, msg)
			}
			mutex.Unlock()
		}
	}()

	http.ListenAndServe(":8080", nil)
}

func (g *Game) Layout(w, h int) (int, int) {
	return screenWidth, screenHeight
}

func main() {
	rand.Seed(time.Now().UnixNano())

	fmt.Print("Name: ")
	var name string
	fmt.Scanln(&name)

	fmt.Print("Host? (y/n): ")
	var choice string
	fmt.Scanln(&choice)

	isHost := choice == "y"
	addr := "localhost:8080"

	if isHost {
		go startServer()
		time.Sleep(time.Second)
	} else {
		fmt.Print("Host IP: ")
		fmt.Scanln(&addr)
	}

	conn, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws", nil)
	if err != nil {
		log.Fatal(err)
	}

	game := &Game{
		conn:    conn,
		MyName:  name,
		IsHost:  isHost,
		Players: make(map[string]*Worm),
		visuals: make(map[string]*VisualData),
	}

	game.Players[name] = &Worm{
		X:     500,
		Y:     100,
		HP:    100,
		Color: color.RGBA{100, 255, 100, 255},
	}

	if isHost {
		h := make([]int, screenWidth)
		for i := range h {
			h[i] = 700 + int(math.Sin(float64(i)*0.01)*50)
		}
		game.Terrain = &Terrain{Heights: h}
	}

	game.groundTexture = createGroundTexture()
	game.skyGradient = createSkyGradient()

	go game.ReceiveLoop()

	ebiten.SetWindowSize(1280, 720)
	ebiten.SetWindowTitle("Worms Battle")
	if err := ebiten.RunGame(game); err != nil {
		log.Fatal(err)
	}
}
