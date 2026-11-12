package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	sleepingBagID = 1263
)

var (
	bankersID = []int{95, 224, 268, 485, 540, 617, 792}
)

func (c *client) getInventoryCountByID(ids ...int) int {
	count := 0
	for i := 0; i < c.inventoryCount; i++ {
		if inIntArray(c.inventory[i].id, ids) {
			count += c.inventory[i].amount
		}
	}
	return count
}

func (c *client) getInventoryItemByID(ids ...int) *item {
	for _, id := range ids {
		for i := 0; i < c.inventoryCount; i++ {
			if c.inventory[i].id == id {
				return c.inventory[i]
			}
		}
	}
	return nil
}

func (c *client) isItemEquippedByID(id int) bool {
	for i := 0; i < c.inventoryCount; i++ {
		if c.inventory[i].id == id && c.inventory[i].equipped {
			return true
		}
	}
	return false
}

// useSleepingBag reports whether a bag was carried and used; with none,
// nothing is sent.
func (c *client) useSleepingBag() bool {
	bag := c.getInventoryItemByID(sleepingBagID)
	if bag == nil {
		fmt.Printf("[%s] Attempted to use nonexistent sleeping bag\n", c.user)
		return false
	}
	c.useInventoryItem(bag)
	return true
}

func (c *player) isSkilling() bool {
	return time.Since(c.skillTime) < c.skillingTimeout
}

func (c *quest) isComplete() bool {
	return c.stage < 0
}

func (c *client) getHPPercent() int {
	return int((float64(c.currentStats[3]) / float64(c.baseStats[3])) * 100)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func max16(a, b int16) int16 {
	if a > b {
		return a
	}
	return b
}

func distance(x1, z1, x2, z2 int) int {
	dx := abs(x1 - x2)
	dy := abs(z1 - z2)
	return max(dx, dy)
}

func distance16(x1, z1, x2, z2 int16) int16 {
	dx := abs16(x1 - x2)
	dy := abs16(z1 - z2)
	return max16(dx, dy)
}

func (c *client) distanceTo(x, z int) int {
	return distance(x, z, c.x, c.z)
}

func (c *client) inRadiusOf(x, z int, radius int) bool {
	return c.distanceTo(x, z) <= radius
}

func (c *client) getObjectFromCoords(x, z int) *object {
	for i := 0; i < c.objectCount; i++ {
		if c.objects[i].x == x && c.objects[i].z == z {
			return c.objects[i]
		}
	}

	return nil
}

func (c *client) getWallObjectFromCoords(x, z int) *wallObject {
	for i := 0; i < c.wallObjectCount; i++ {
		if c.wallObjects[i].x == x && c.wallObjects[i].z == z {
			return c.wallObjects[i]
		}
	}

	return nil
}

func (c *client) isReachable(x, z int) bool {
	if c.x == x && c.z == z {
		return true
	}
	l := c.findPath(c.x, c.z, x, x, z, z, false)
	return l != -1
}

func (c *client) isGroundItemReachable(g *groundItem) bool {
	l := c.findPath(c.x, c.z, g.x, g.x, g.z, g.z, false)
	if l == -1 {
		l = c.findPath(c.x, c.z, g.x, g.x, g.z, g.z, true)
		return l != -1
	}
	return true
}

func (c *client) isWallObjectReachable(w *wallObject) bool {
	if w.dir == 0 {
		l := c.findPath(c.x, c.z, w.x, w.x, w.z-1, w.z, false)
		return l != -1
	} else if w.dir != 1 {
		l := c.findPath(c.x, c.z, w.x, w.x, w.z, w.z, true)
		return l != -1
	} else {
		l := c.findPath(c.x, c.z, w.x-1, w.x, w.z, w.z, false)
		return l != -1
	}
}

func (c *client) isObjectReachable(o *object) bool {
	var (
		objWidth  int
		objHeight int
	)
	def := objectDefs[o.id]
	if o.dir != 0 && o.dir != 4 {
		objWidth = int(def.Height)
		objHeight = int(def.Width)
	} else {
		objHeight = int(def.Height)
		objWidth = int(def.Width)
	}
	destX := o.x
	destZ := o.z

	if def.Typ != 2 && def.Typ != 3 {
		l := c.findPath(c.x, c.z, o.x, objWidth-1+destX, o.z, destZ+objHeight-1, true)
		return l != -1
	} else {
		if o.dir == 0 {
			objWidth++
			destX--
		}

		if o.dir == 2 {
			objHeight++
		}

		if o.dir == 6 {
			destZ--
			objHeight++
		}

		if o.dir == 4 {
			objWidth++
		}

		l := c.findPath(c.x, c.z, destX, objWidth+(destX-1), destZ, objHeight+destZ-1, false)
		return l != -1
	}
}

func (n *npc) inCombat() bool {
	return n.sprite == 8 || n.sprite == 9
}

func (c *client) inCombat() bool {
	return c.sprite == 8 || c.sprite == 9
}

func (n *npc) isTalking() bool {
	return time.Since(n.messageTime) < n.lastMessageTimeout
}

func (n *player) isTalking() bool {
	return time.Since(n.messageTime) < n.lastMessageTimeout
}

func (p *player) inCombat() bool {
	return p.sprite == 8 || p.sprite == 9
}

func (c *client) inRect(x, z, width, height int) bool {
	return inRect(c.x, c.z, x, z, width, height)
}

func inRect(x1, z1, x2, z2, width, height int) bool {
	return x1 <= x2 && z1 >= z2 && x1 > x2-width && z1 < z2+height
}

func inIntArray(needle int, haystack []int) bool {
	for _, n := range haystack {
		if needle == n {
			return true
		}
	}
	return false
}

func (c *client) getNearestNPCByID(inCombat, talking, reachable bool, x, z int, radius int, ids ...int) *npc {
	var (
		min    = math.MaxInt32
		minNPC *npc
	)
	for i := 0; i < c.npcCount; i++ {
		if !inIntArray(c.npcs[i].id, ids) {
			continue
		}
		if dist := distance(c.x, c.z, c.npcs[i].x, c.npcs[i].z); dist < min {
			if !inCombat && c.npcs[i].inCombat() {
				continue
			}
			if !talking && c.npcs[i].isTalking() {
				continue
			}
			if radius != -1 && distance(c.npcs[i].x, c.npcs[i].z, x, z) > radius {
				continue
			}
			if reachable && !c.isReachable(c.npcs[i].x, c.npcs[i].z) {
				continue
			}
			minNPC = c.npcs[i]
			min = dist
		}
	}
	return minNPC
}

func (c *client) getNearestNPCByIDInRect(inCombat bool, talking bool, reachable bool, x, z, width, height int, ids ...int) *npc {
	var (
		min    = math.MaxInt32
		minNPC *npc
	)
	for i := 0; i < c.npcCount; i++ {
		if !inIntArray(c.npcs[i].id, ids) {
			continue
		}
		if dist := distance(c.x, c.z, c.npcs[i].x, c.npcs[i].z); dist < min {
			if !inCombat && c.npcs[i].inCombat() {
				continue
			}
			if !talking && c.npcs[i].isTalking() {
				continue
			}
			if !inRect(c.npcs[i].x, c.npcs[i].z, x, z, width, height) {
				continue
			}
			if reachable && !c.isReachable(c.npcs[i].x, c.npcs[i].z) {
				continue
			}
			minNPC = c.npcs[i]
			min = dist
		}
	}
	return minNPC
}

func (c *client) getNearestObjectByIDInRect(reachable bool, x, z, width, height int, ids ...int) *object {
	var (
		min    = math.MaxInt32
		minObj *object
	)
	for i := 0; i < c.objectCount; i++ {
		if !inIntArray(c.objects[i].id, ids) {
			continue
		}
		if dist := distance(c.x, c.z, c.objects[i].x, c.objects[i].z); dist < min {
			if !inRect(c.objects[i].x, c.objects[i].z, x, z, width, height) {
				continue
			}
			if reachable && !c.isObjectReachable(c.objects[i]) {
				continue
			}
			minObj = c.objects[i]
			min = dist
		}
	}
	return minObj
}

func (c *client) getNearestWallObjectByIDInRect(reachable bool, x, z, width, height int, ids ...int) *wallObject {
	var (
		min    = math.MaxInt32
		minObj *wallObject
	)
	for i := 0; i < c.wallObjectCount; i++ {
		if !inIntArray(c.wallObjects[i].id, ids) {
			continue
		}
		if dist := distance(c.x, c.z, c.wallObjects[i].x, c.wallObjects[i].z); dist < min {
			if !inRect(c.wallObjects[i].x, c.wallObjects[i].z, x, z, width, height) {
				continue
			}
			if reachable && !c.isWallObjectReachable(c.wallObjects[i]) {
				continue
			}
			minObj = c.wallObjects[i]
			min = dist
		}
	}
	return minObj
}

func (c *client) getNearestGroundItemByIDInRect(reachable bool, x, z, width, height int, ids ...int) *groundItem {
	var (
		min    = math.MaxInt32
		minObj *groundItem
	)
	for i := 0; i < c.groundItemCount; i++ {
		if !inIntArray(c.groundItems[i].id, ids) {
			continue
		}
		if dist := distance(c.x, c.z, c.groundItems[i].x, c.groundItems[i].z); dist < min {
			if !inRect(c.groundItems[i].x, c.groundItems[i].z, x, z, width, height) {
				continue
			}
			if reachable && !c.isGroundItemReachable(c.groundItems[i]) {
				continue
			}
			minObj = c.groundItems[i]
			min = dist
		}
	}
	return minObj
}

func (c *client) getNearestGroundItemByID(x, z int, radius int, reachable bool, ids ...int) *groundItem {
	var (
		min     = math.MaxInt32
		minItem *groundItem
	)
	for i := 0; i < c.groundItemCount; i++ {
		if !inIntArray(c.groundItems[i].id, ids) {
			continue
		}
		if dist := distance(c.x, c.z, c.groundItems[i].x, c.groundItems[i].z); dist < min {
			if radius != -1 && distance(c.groundItems[i].x, c.groundItems[i].z, x, z) > radius {
				continue
			}
			if reachable && !c.isGroundItemReachable(c.groundItems[i]) {
				continue
			}
			minItem = c.groundItems[i]
			min = dist
		}
	}
	return minItem
}

func (c *client) getNearestObjectByID(x, z int, radius int, reachable bool, ids ...int) *object {
	var (
		min    = math.MaxInt32
		minObj *object
	)
	for i := 0; i < c.objectCount; i++ {
		if !inIntArray(c.objects[i].id, ids) {
			continue
		}
		if dist := distance(c.x, c.z, c.objects[i].x, c.objects[i].z); dist < min {
			if radius != -1 && distance(c.objects[i].x, c.objects[i].z, x, z) > radius {
				continue
			}
			if reachable && !c.isObjectReachable(c.objects[i]) {
				continue
			}
			minObj = c.objects[i]
			min = dist
		}
	}
	return minObj
}

func (c *client) getNearestWallObjectByID(x, z int, radius int, reachable bool, ids ...int) *wallObject {
	var (
		min    = math.MaxInt32
		minObj *wallObject
	)
	for i := 0; i < c.wallObjectCount; i++ {
		if !inIntArray(c.wallObjects[i].id, ids) {
			continue
		}
		if dist := distance(c.x, c.z, c.wallObjects[i].x, c.wallObjects[i].z); dist < min {
			if radius != -1 && distance(c.wallObjects[i].x, c.wallObjects[i].z, x, z) > radius {
				continue
			}
			if reachable && !c.isWallObjectReachable(c.wallObjects[i]) {
				continue
			}
			minObj = c.wallObjects[i]
			min = dist
		}
	}
	return minObj
}

func (c *client) bankCount(ids ...int) int {
	count := 0
	for i := 0; i < c.bankItemCount; i++ {
		if inIntArray(c.bankItems[i].id, ids) {
			count += c.bankItems[i].amount
		}
	}
	return count
}

func (c *client) hasBankItem(id int) bool {
	return c.bankCount(id) > 0
}

func (c *client) getPlayerByName(name string) *player {
	name = strings.ToLower(name)
	for i := 0; i < c.playerCount; i++ {
		if strings.ToLower(c.players[i].username) == name {
			return c.players[i]
		}
	}
	return nil
}

func (c *client) isGroundItemAt(id, x, z int) bool {
	for i := 0; i < c.groundItemCount; i++ {
		if c.groundItems[i].id == id && c.groundItems[i].x == x && c.groundItems[i].z == z {
			return true
		}
	}
	return false
}

func getTradeItemCount(items [14]*tradeItem, length int, id int) int {
	count := 0
	for i := 0; i < length; i++ {
		v := items[i]
		if v.id == id {
			count += v.amount
		}
	}
	return count
}

func (c *client) hasRecipientOffer(id, amount int) bool {
	return getTradeItemCount(c.recipientTradeItems, c.recipientTradeCount, id) >= amount
}

func (c *client) hasRecipientConfirm(id, amount int) bool {
	return getTradeItemCount(c.recipientConfirmItems, c.recipientConfirmItemCount, id) >= amount
}

func (c *client) hasTradeOffer(id, amount int) bool {
	return getTradeItemCount(c.myTradeItems, c.myTradeCount, id) >= amount
}

func (c *client) hasTradeConfirm(id, amount int) bool {
	return getTradeItemCount(c.myTradeConfirmItems, c.myTradeConfirmItemCount, id) >= amount
}

func (c *client) isObjectAt(x, z int) bool {
	for i := 0; i < c.objectCount; i++ {
		if c.objects[i].x == x && c.objects[i].z == z {
			return true
		}
	}
	return false
}

func (c *client) isWallObjectAt(x, z int) bool {
	for i := 0; i < c.wallObjectCount; i++ {
		if c.wallObjects[i].x == x && c.wallObjects[i].z == z {
			return true
		}
	}
	return false
}

func (c *client) getShopItemByID(id int) *shopItem {
	for i := 0; i < c.shopItemCount; i++ {
		if c.shopItems[i].id == id {
			return c.shopItems[i]
		}
	}
	return nil
}

func (c *client) isFriend(name string) bool {
	name = strings.ToLower(name)
	for i := 0; i < c.friendListCount; i++ {
		if strings.ToLower(c.friendList[i].username) == name {
			return true
		}
	}
	return false
}

func (c *client) isIgnored(name string) bool {
	name = strings.ToLower(name)
	for i := 0; i < c.ignoreListCount; i++ {
		if strings.ToLower(c.ignoreList[i].username) == name {
			return true
		}
	}
	return false
}

func (c *client) getShopAmount(id int) int {
	for i := 0; i < c.shopItemCount; i++ {
		if c.shopItems[i].id == id {
			return c.shopItems[i].amount
		}
	}
	return 0
}

func (c *client) generateLongPathPoint(x, z, depth int16) *point {
	if depth == -1 {
		depth = int16(distance16(int16(c.x), int16(c.z), x, z) * 4)
	}

	if depth > 800 {
		return nil
	}

	p := newAstarPathFinder(c, &point{int16(c.x), int16(c.z)}, &point{x, z}, depth, false)

	path := p.findPath()

	if path == nil {
		return nil
	}

	path.waypoints.PushBack(&point{x, z})

	stepSize := 10
	var pt *point
	if path.waypoints.Len() <= stepSize {
		pt = path.waypoints.Back()
	} else {
		pts := make([]*point, stepSize)
		for i := 0; i < stepSize; i++ {
			pts[i] = path.waypoints.PopFront()
		}
		pt = pts[stepSize-1]
	}

	return pt
}

func (c *client) generateLongPath(startX, startZ, x, z, depth int16, skipLocal bool, maxDepth int16) (*longPath, []*point) {
	var path *path
	if depth == -1 {
		depth = int16(math.Pow(2, math.Ceil(math.Log2(float64(distance16(startX, startZ, x, z))))))
		for {
			if depth > maxDepth {
				return nil, nil
			}
			p := newAstarPathFinder(c, &point{startX, startZ}, &point{x, z}, depth, skipLocal)
			path = p.findPath()

			if path != nil {
				break
			}
			depth <<= 1
		}
	} else {
		p := newAstarPathFinder(c, &point{startX, startZ}, &point{x, z}, depth, skipLocal)
		path = p.findPath()

		if path == nil {
			return nil, nil
		}
	}

	path.waypoints.PushBack(&point{x, z})

	stepSize := 10

	lp := &longPath{
		points:   []*point{},
		script:   c.script,
		length:   path.waypoints.Len(),
		stepSize: stepSize,
	}

	coords := make([]*point, path.waypoints.Len())
	for i := 0; i < lp.length; i++ {
		coords[i] = path.waypoints.PopFront()
	}

	if lp.length <= stepSize {
		lp.points = append(lp.points, coords[len(coords)-1])
	} else {
		for i := stepSize - 1; i < len(coords); i += stepSize {
			lp.points = append(lp.points, coords[i])
		}
		if (len(coords) % stepSize) != 0 {
			lp.points = append(lp.points, coords[len(coords)-1])
		}
	}

	return lp, coords
}

func (c *client) generateLongPathThrough(startX, startZ int16, coords []*point, depth int16, skipLocal bool, maxDepth int16) (*longPath, []*point, *point, *point) {
	totalPath := []*point{}
	lastX, lastZ := startX, startZ
	initialDepth := depth
	for _, v := range coords {
		x, z := v.x, v.y
		var path *path
		if initialDepth == -1 {
			depth = int16(math.Pow(2, math.Ceil(math.Log2(float64(distance16(lastX, lastZ, x, z))))))
			for {
				if depth > maxDepth {
					return nil, nil, &point{lastX, lastZ}, &point{x, z}
				}
				p := newAstarPathFinder(c, &point{lastX, lastZ}, &point{x, z}, depth, skipLocal)
				path = p.findPath()

				if path != nil {
					break
				}
				depth <<= 1
			}
		} else {
			p := newAstarPathFinder(c, &point{lastX, lastZ}, &point{x, z}, initialDepth, skipLocal)
			path = p.findPath()

			if path == nil {
				return nil, nil, &point{lastX, lastZ}, &point{x, z}
			}
		}

		path.waypoints.PushBack(&point{x, z})

		for path.waypoints.Len() > 0 {
			totalPath = append(totalPath, path.waypoints.PopFront())
		}

		lastX = x
		lastZ = z
	}

	stepSize := 10

	lp := &longPath{
		points:   []*point{},
		script:   c.script,
		length:   len(totalPath),
		stepSize: stepSize,
	}

	if len(totalPath) <= stepSize {
		pt := totalPath[len(totalPath)-1]
		lp.points = append(lp.points, pt)
	} else {
		count := 1
		for _, p := range totalPath {
			if count%stepSize == 0 {
				lp.points = append(lp.points, p)
			}
			count++
		}
		if (count-1)%stepSize != 0 {
			pt := totalPath[len(totalPath)-1]
			lp.points = append(lp.points, pt)
		}
	}

	return lp, totalPath, nil, nil
}
