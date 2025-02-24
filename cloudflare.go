package main

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudflare/cloudflare-go"
	"github.com/crowdsecurity/crowdsec/pkg/models"
	"github.com/prometheus/client_golang/prometheus"

	log "github.com/sirupsen/logrus"
)

const CallsPerSecondLimit uint32 = 4

var CloudflareActionByDecisionType = map[string]string{
	"captcha":      "challenge",
	"ban":          "block",
	"js_challenge": "js_challenge",
}

type ZoneLock struct {
	Lock   *sync.Mutex
	ZoneID string
}

type IPListState struct {
	IPList   *cloudflare.IPList
	ItemByIP map[string]cloudflare.IPListItem
}

// one firewall rule per zone.
type CloudflareState struct {
	Action              string
	AccountID           string
	FilterIDByZoneID    map[string]string // this contains all the zone ID -> filter ID which represent this state
	CurrExpr            string
	IPListState         IPListState
	CountrySet          map[string]struct{}
	AutonomousSystemSet map[string]struct{}
}

func setToExprList(set map[string]struct{}, quotes bool) string {
	items := make([]string, len(set))
	i := 0
	for str := range set {
		if quotes {
			items[i] = fmt.Sprintf(`"%s"`, str)
		} else {
			items[i] = str
		}
		i++
	}
	sort.Strings(items)
	return fmt.Sprintf("{%s}", strings.Join(items, " "))
}

func allZonesHaveAction(zones []ZoneConfig, action string) bool {
	allSupport := true
	for _, zone := range zones {
		if _, allSupport = zone.ActionSet[action]; !allSupport {
			break
		}
	}
	return allSupport
}

func (cfState CloudflareState) computeExpression() string {
	var countryExpr, ASExpr, ipExpr string
	buff := make([]string, 0)

	if len(cfState.CountrySet) > 0 {
		countryExpr = fmt.Sprintf("(ip.geoip.country in %s)", setToExprList(cfState.CountrySet, true))
		buff = append(buff, countryExpr)
	}

	if len(cfState.AutonomousSystemSet) > 0 {
		ASExpr = fmt.Sprintf("(ip.geoip.asnum in %s)", setToExprList(cfState.AutonomousSystemSet, false))
		buff = append(buff, ASExpr)
	}

	if cfState.IPListState.IPList != nil {
		ipExpr = fmt.Sprintf("(ip.src in $%s)", cfState.IPListState.IPList.Name)
		buff = append(buff, ipExpr)
	}

	return strings.Join(buff, " or ")
}

// updates the expression for the state. Returns true if new rule is
// different than the previous rule.
func (cfState *CloudflareState) UpdateExpr() bool {
	computedExpr := cfState.computeExpression()
	isNew := computedExpr != cfState.CurrExpr
	cfState.CurrExpr = computedExpr
	return isNew
}

type CloudflareWorker struct {
	Logger                  *log.Entry
	Account                 AccountConfig
	ZoneLocks               []ZoneLock
	CFStateByAction         map[string]*CloudflareState
	Ctx                     context.Context
	LAPIStream              chan *models.DecisionsStreamResponse
	UpdatedState            chan map[string]*CloudflareState
	UpdateFrequency         time.Duration
	NewIPDecisions          []*models.Decision
	ExpiredIPDecisions      []*models.Decision
	NewASDecisions          []*models.Decision
	ExpiredASDecisions      []*models.Decision
	NewCountryDecisions     []*models.Decision
	ExpiredCountryDecisions []*models.Decision
	API                     cloudflareAPI
	Wg                      *sync.WaitGroup
	Count                   prometheus.Counter
	tokenCallCount          *uint32
}

type cloudflareAPI interface {
	Filters(ctx context.Context, zoneID string, pageOpts cloudflare.PaginationOptions) ([]cloudflare.Filter, error)
	ListZones(ctx context.Context, z ...string) ([]cloudflare.Zone, error)
	CreateIPList(ctx context.Context, name string, desc string, typ string) (cloudflare.IPList, error)
	DeleteIPList(ctx context.Context, id string) (cloudflare.IPListDeleteResponse, error)
	ListIPLists(ctx context.Context) ([]cloudflare.IPList, error)
	CreateFirewallRules(ctx context.Context, zone string, rules []cloudflare.FirewallRule) ([]cloudflare.FirewallRule, error)
	DeleteFirewallRules(ctx context.Context, zoneID string, firewallRuleIDs []string) error
	FirewallRules(ctx context.Context, zone string, opts cloudflare.PaginationOptions) ([]cloudflare.FirewallRule, error)
	CreateIPListItems(ctx context.Context, id string, items []cloudflare.IPListItemCreateRequest) ([]cloudflare.IPListItem, error)
	DeleteIPListItems(ctx context.Context, id string, items cloudflare.IPListItemDeleteRequest) ([]cloudflare.IPListItem, error)
	DeleteFilters(ctx context.Context, zoneID string, filterIDs []string) error
	UpdateFilters(ctx context.Context, zoneID string, filters []cloudflare.Filter) ([]cloudflare.Filter, error)
}

func min(a int, b int) int {
	if a > b {
		return b
	}
	return a
}

func normalizeDecisionValue(value string) string {
	if strings.Count(value, ":") <= 1 {
		// it is a ipv4
		return value
	}
	var address *net.IPNet
	_, address, err := net.ParseCIDR(value)
	if err != nil {
		// doesn't have mask, we add one then.
		_, address, _ = net.ParseCIDR(value + "/64")
		// this would never cause error because crowdsec already validates IP
	}

	if ones, _ := address.Mask.Size(); ones < 64 {
		return address.String()
	}
	address.Mask = net.CIDRMask(64, 128)
	return address.String()
}

// Helper which removes dups and splits decisions according to their action.
// Decisions with unsupported action are ignored
func dedupAndClassifyDecisionsByAction(decisions []*models.Decision) map[string][]*models.Decision {
	decisionValueSet := make(map[string]struct{})
	decisonsByAction := make(map[string][]*models.Decision)
	tmpDefaulted := make([]*models.Decision, 0)
	for _, decision := range decisions {
		*decision.Value = normalizeDecisionValue(*decision.Value)
		action := CloudflareActionByDecisionType[*decision.Type]
		if _, ok := decisionValueSet[*decision.Value]; ok {
			// dup
			continue
		}
		if action == "" {
			// unsupported decision type, ignore this if in case decision with supported action
			// for the same decision value is present.
			tmpDefaulted = append(tmpDefaulted, decision)
			continue
		} else {
			decisionValueSet[*decision.Value] = struct{}{}
		}
		decisonsByAction[action] = append(decisonsByAction[action], decision)
	}
	defaulted := make([]*models.Decision, 0)
	for _, decision := range tmpDefaulted {
		if _, ok := decisionValueSet[*decision.Value]; ok {
			// dup
			continue
		}
		defaulted = append(defaulted, decision)
	}
	decisonsByAction["defaulted"] = defaulted
	return decisonsByAction
}

// getters
func (worker *CloudflareWorker) getMutexByZoneID(zoneID string) (*sync.Mutex, error) {
	for _, zoneLock := range worker.ZoneLocks {
		if zoneLock.ZoneID == zoneID {
			return zoneLock.Lock, nil
		}
	}
	return nil, fmt.Errorf("zone lock for the zone id %s not found", zoneID)
}

func (worker *CloudflareWorker) getAPI() cloudflareAPI {
	atomic.AddUint32(worker.tokenCallCount, 1)
	if *worker.tokenCallCount > CallsPerSecondLimit {
		time.Sleep(time.Second)
	}
	worker.Count.Inc()
	return worker.API
}

func (worker *CloudflareWorker) deleteRulesContainingStringFromZoneIDs(str string, zonesIDs []string) error {
	for _, zoneID := range zonesIDs {
		zoneLogger := worker.Logger.WithFields(log.Fields{"zone_id": zoneID})
		zoneLock, err := worker.getMutexByZoneID(zoneID)
		if err == nil {
			zoneLock.Lock()
			defer zoneLock.Unlock()
		}
		rules, err := worker.getAPI().FirewallRules(worker.Ctx, zoneID, cloudflare.PaginationOptions{})
		if err != nil {
			return err
		}
		deleteRules := make([]string, 0)

		for _, rule := range rules {
			if strings.Contains(rule.Filter.Expression, str) {
				deleteRules = append(deleteRules, rule.ID)
			}
		}
		if len(deleteRules) > 0 {
			err = worker.getAPI().DeleteFirewallRules(worker.Ctx, zoneID, deleteRules)
			if err != nil {
				return err
			}
			zoneLogger.Infof("deleted %d firewall rules containing the string %s", len(deleteRules), str)
		}

	}
	return nil
}

func (worker *CloudflareWorker) deleteFiltersContainingStringFromZoneIDs(str string, zonesIDs []string) error {
	for _, zoneID := range zonesIDs {
		zoneLogger := worker.Logger.WithFields(log.Fields{"zone_id": zoneID})
		zoneLock, err := worker.getMutexByZoneID(zoneID)
		if err == nil {
			zoneLock.Lock()
			defer zoneLock.Unlock()
		}
		filters, err := worker.getAPI().Filters(worker.Ctx, zoneID, cloudflare.PaginationOptions{})
		if err != nil {
			return err
		}
		deleteFilters := make([]string, 0)
		for _, filter := range filters {
			if strings.Contains(filter.Expression, str) {
				deleteFilters = append(deleteFilters, filter.ID)
				zoneLogger.Debugf("deleting %s filter with expression %s", filter.ID, filter.Expression)
			}
		}

		if len(deleteFilters) > 0 {
			zoneLogger.Infof("deleting %d filters", len(deleteFilters))
			err = worker.getAPI().DeleteFilters(worker.Ctx, zoneID, deleteFilters)
			if err != nil {
				return err
			}
		}

	}
	return nil
}

func (worker *CloudflareWorker) deleteExistingIPList() error {
	IPLists, err := worker.getAPI().ListIPLists(worker.Ctx)
	if err != nil {
		return err
	}

	for _, state := range worker.CFStateByAction {
		IPList := state.IPListState.IPList
		id := worker.getIPListID(IPList.Name, IPLists) // requires ip list name
		if id == nil {
			worker.Logger.Infof("ip list %s does not exists", IPList.Name)
			continue
		}

		worker.Logger.Infof("ip list %s already exists", IPList.Name)
		err = worker.removeIPListDependencies(IPList.Name) // requires ip list name
		if err != nil {
			return err
		}

		_, err = worker.getAPI().DeleteIPList(worker.Ctx, *id)
		if err != nil {
			return err
		}
	}
	return nil
}

func (worker *CloudflareWorker) removeIPListDependencies(IPListName string) error {
	zones, err := worker.getAPI().ListZones(worker.Ctx)
	if err != nil {
		return err
	}

	zoneIDs := make([]string, len(zones))
	for i, zone := range zones {
		zoneIDs[i] = zone.ID
	}

	worker.Logger.Debugf("found %d zones on this account", len(zones))
	err = worker.deleteRulesContainingStringFromZoneIDs(fmt.Sprintf("$%s", IPListName), zoneIDs)
	if err != nil {
		return err
	}
	// A Filter can exist on it's own, they are not visible on UI, they are API only.
	// Clear these Filters.
	err = worker.deleteFiltersContainingStringFromZoneIDs(fmt.Sprintf("$%s", IPListName), zoneIDs)
	if err != nil {
		return err
	}
	return nil
}

func (worker *CloudflareWorker) getIPListID(IPListName string, IPLists []cloudflare.IPList) *string {
	for _, ipList := range IPLists {
		if ipList.Name == IPListName {
			return &ipList.ID
		}
	}
	return nil
}

func (worker *CloudflareWorker) setUpIPList() error {
	err := worker.deleteExistingIPList()
	if err != nil {
		return err
	}

	for action, state := range worker.CFStateByAction {
		ipList := *state.IPListState.IPList
		tmp, err := worker.getAPI().CreateIPList(worker.Ctx, ipList.Name, fmt.Sprintf("%s IP list by crowdsec", action), "ip")
		if err != nil {
			return err
		}
		*worker.CFStateByAction[action].IPListState.IPList = tmp
		worker.CFStateByAction[action].IPListState.ItemByIP = make(map[string]cloudflare.IPListItem)
		worker.CFStateByAction[action].UpdateExpr()

	}
	return nil
}

func (worker *CloudflareWorker) setUpRules() error {
	for _, zone := range worker.Account.ZoneConfigs {
		zoneLogger := worker.Logger.WithFields(log.Fields{"zone_id": zone.ID})
		for _, action := range zone.Actions {
			ruleExpression := worker.CFStateByAction[action].CurrExpr
			firewallRules := []cloudflare.FirewallRule{{Filter: cloudflare.Filter{Expression: ruleExpression}, Action: action, Description: fmt.Sprintf("CrowdSec %s rule", action)}}
			rule, err := worker.getAPI().CreateFirewallRules(worker.Ctx, zone.ID, firewallRules)
			if err != nil {
				worker.Logger.WithFields(log.Fields{"zone_id": zone.ID}).Errorf("error %s in creating firewall rule %s", err.Error(), ruleExpression)
				return err
			}
			worker.CFStateByAction[action].FilterIDByZoneID[zone.ID] = rule[0].Filter.ID
		}
		zoneLogger.Info("firewall rules created")
	}
	worker.Logger.Info("setup of firewall rules complete")
	return nil
}

func (worker *CloudflareWorker) AddNewIPs() error {
	// IP decisions are applied at account level
	decisonsByAction := dedupAndClassifyDecisionsByAction(worker.NewIPDecisions)
	for action, decisions := range decisonsByAction {
		// In case some zones support this action and others don't,  we put this in account's default action.
		if !allZonesHaveAction(worker.Account.ZoneConfigs, action) {
			if worker.Account.DefaultAction == "none" {
				worker.Logger.Debugf("dropping IP decisions with unsupported action %s", action)
				continue
			}
			action = worker.Account.DefaultAction
			worker.Logger.Debugf("ip action defaulted to %s", action)
		}
		state := worker.CFStateByAction[action]
		newIPs := make([]cloudflare.IPListItemCreateRequest, 0)
		for _, decision := range decisions {
			// check if ip already exists in state. Send if not exists.
			ip := normalizeDecisionValue(*decision.Value)
			if _, ok := state.IPListState.ItemByIP[ip]; !ok {
				newIPs = append(newIPs, cloudflare.IPListItemCreateRequest{
					IP:      ip,
					Comment: *decision.Scenario,
				})
				worker.CFStateByAction[action].IPListState.IPList.NumItems++
			}
		}
		if len(newIPs) > 0 {
			items, err := worker.getAPI().CreateIPListItems(worker.Ctx, state.IPListState.IPList.ID, newIPs)
			if err != nil {
				return err
			}
			worker.Logger.Infof("banned %d IPs", len(newIPs))
			for _, item := range items {
				worker.CFStateByAction[action].IPListState.ItemByIP[item.IP] = item
			}
		}

	}
	go func() { worker.UpdatedState <- worker.CFStateByAction }()
	worker.NewIPDecisions = make([]*models.Decision, 0)
	return nil
}

func (worker *CloudflareWorker) DeleteIPs() error {
	// IP decisions are applied at account level
	decisonsByAction := dedupAndClassifyDecisionsByAction(worker.ExpiredIPDecisions)
	for action, decisions := range decisonsByAction {
		// In case some zones support this action and others don't,  we put this in account's default action.
		if !allZonesHaveAction(worker.Account.ZoneConfigs, action) {
			if worker.Account.DefaultAction == "none" {
				worker.Logger.Debugf("dropping IP delete decisions with unsupported action %s", action)
				continue
			}
			action = worker.Account.DefaultAction
			worker.Logger.Debugf("ip delete action defaulted to %s", action)

		}
		state := worker.CFStateByAction[action]
		deleteIPs := cloudflare.IPListItemDeleteRequest{Items: make([]cloudflare.IPListItemDeleteItemRequest, 0)}
		for _, decision := range decisions {
			// delete only if ip already exists in state.
			ip := normalizeDecisionValue(*decision.Value)
			if item, ok := state.IPListState.ItemByIP[ip]; ok {
				deleteIPs.Items = append(deleteIPs.Items, cloudflare.IPListItemDeleteItemRequest{ID: item.ID})
			}
		}

		if len(deleteIPs.Items) > 0 {
			_, err := worker.getAPI().DeleteIPListItems(worker.Ctx, state.IPListState.IPList.ID, deleteIPs)
			if err != nil {
				return err
			}
			worker.CFStateByAction[action].IPListState.IPList.NumItems -= len(deleteIPs.Items)
			ipByID := make(map[string]string)
			for ip, item := range worker.CFStateByAction[action].IPListState.ItemByIP {
				ipByID[item.ID] = ip
			}
			for _, item := range deleteIPs.Items {
				delete(worker.CFStateByAction[action].IPListState.ItemByIP, ipByID[item.ID])
			}
		}

	}
	go func() { worker.UpdatedState <- worker.CFStateByAction }()
	worker.ExpiredIPDecisions = make([]*models.Decision, 0)
	return nil
}

func (worker *CloudflareWorker) stateIsNew() bool {

	isNew := false
	for _, action := range worker.CFStateByAction {
		// this means that ip list is not created, hence the state is new.
		if action.IPListState.IPList.CreatedOn == nil {
			isNew = true
			break
		}
	}
	return isNew
}

func (worker *CloudflareWorker) SetUpCloudflareIfNewState() error {

	defer func() { worker.UpdatedState <- worker.CFStateByAction }()

	if !worker.stateIsNew() {
		worker.Logger.Info("state hasn't changed, not setting up CF")
		worker.Wg.Done()
		return nil
	}

	err := worker.setUpIPList()
	if err != nil {
		worker.Logger.Errorf("error %s in creating IP List", err.Error())
		return err
	}
	worker.Wg.Done()
	worker.Wg.Wait()
	worker.Logger.Debug("ip list setup complete")
	err = worker.setUpRules()
	if err != nil {
		worker.Logger.Error(err.Error())
		return err
	}
	return nil
}

func (worker *CloudflareWorker) Init() error {

	defer func() { worker.UpdatedState <- worker.CFStateByAction }()

	var err error

	worker.Logger = log.WithFields(log.Fields{"account_id": worker.Account.ID})
	worker.NewIPDecisions = make([]*models.Decision, 0)
	worker.ExpiredIPDecisions = make([]*models.Decision, 0)

	if worker.API == nil { // this for easy swapping during tests
		worker.API, err = cloudflare.NewWithAPIToken(worker.Account.Token, cloudflare.UsingAccount(worker.Account.ID))
	}

	worker.Logger.Debug("setup of API complete")

	if len(worker.CFStateByAction) != 0 {
		// no  need to  setup ip lists and rules, since cache is being used.
		return nil
	}

	worker.CFStateByAction = make(map[string]*CloudflareState)

	zones, err := worker.API.ListZones(worker.Ctx)
	if err != nil {
		worker.Logger.Error(err.Error())
		return err
	}
	zoneByID := make(map[string]cloudflare.Zone)
	for _, zone := range zones {
		zoneByID[zone.ID] = zone
	}

	for _, z := range worker.Account.ZoneConfigs {
		if zone, ok := zoneByID[z.ID]; ok {
			// FIXME this is probably wrong.
			if !zone.Plan.IsSubscribed && len(z.Actions) > 1 {
				return fmt.Errorf("zone %s 's plan doesn't support multiple actionss", z.ID)
			}

			for _, action := range z.Actions {
				listName := fmt.Sprintf("%s_%s", worker.Account.IPListPrefix, action)
				worker.CFStateByAction[action] = &CloudflareState{
					AccountID:   worker.Account.ID,
					Action:      action,
					IPListState: IPListState{IPList: &cloudflare.IPList{Name: listName}, ItemByIP: make(map[string]cloudflare.IPListItem)},
				}
				worker.CFStateByAction[action].FilterIDByZoneID = make(map[string]string)
				worker.CFStateByAction[action].CountrySet = make(map[string]struct{})
				worker.CFStateByAction[action].AutonomousSystemSet = make(map[string]struct{})

			}
		} else {
			return fmt.Errorf("account %s doesn't have access to one %s", worker.Account.ID, z.ID)
		}
	}
	return err
}

func (worker *CloudflareWorker) getContainerByDecisionScope(scope string, decisionIsExpired bool) (*([]*models.Decision), error) {
	var containerByDecisionScope map[string]*([]*models.Decision)
	if decisionIsExpired {
		containerByDecisionScope = map[string]*([]*models.Decision){
			"IP":      &worker.ExpiredIPDecisions,
			"RANGE":   &worker.ExpiredIPDecisions, // Cloudflare IP lists handle ranges fine
			"COUNTRY": &worker.ExpiredCountryDecisions,
			"AS":      &worker.ExpiredASDecisions,
		}
	} else {
		containerByDecisionScope = map[string]*([]*models.Decision){
			"IP":      &worker.NewIPDecisions,
			"RANGE":   &worker.NewIPDecisions, // Cloudflare IP lists handle ranges fine
			"COUNTRY": &worker.NewCountryDecisions,
			"AS":      &worker.NewASDecisions,
		}
	}
	scope = strings.ToUpper(scope)
	if container, ok := containerByDecisionScope[scope]; !ok {
		return nil, fmt.Errorf("%s scope is not supported", scope)
	} else {
		return container, nil
	}
}
func (worker *CloudflareWorker) insertDecision(decision *models.Decision, decisionIsExpired bool) {
	container, err := worker.getContainerByDecisionScope(*decision.Scope, decisionIsExpired)
	if err != nil {
		worker.Logger.Debugf("ignored new decision with scope=%s, type=%s, value=%s", *decision.Scope, *decision.Type, *decision.Value)
		return
	}
	decisionStatus := "new"
	if decisionIsExpired {
		decisionStatus = "expired"
	}
	worker.Logger.Infof("found %s decision with value=%s, scope=%s, type=%s", decisionStatus, *decision.Value, *decision.Scope, *decision.Type)
	*container = append(*container, decision)
}

func (worker *CloudflareWorker) CollectLAPIStream(streamDecision *models.DecisionsStreamResponse) {
	for _, decision := range streamDecision.New {
		worker.insertDecision(decision, false)
	}
	for _, decision := range streamDecision.Deleted {
		worker.insertDecision(decision, true)
	}
}

func (worker *CloudflareWorker) SendASBans() error {
	decisionsByAction := dedupAndClassifyDecisionsByAction(worker.NewASDecisions)
	for _, zoneCfg := range worker.Account.ZoneConfigs {
		zoneLogger := worker.Logger.WithFields(log.Fields{"zone_id": zoneCfg.ID})
		for action, decisions := range decisionsByAction {
			action = worker.normalizeActionForZone(action, zoneCfg)
			for _, decision := range decisions {
				if _, ok := worker.CFStateByAction[action].AutonomousSystemSet[*decision.Value]; !ok {
					zoneLogger.Debugf("found new AS ban for %s", *decision.Value)
					worker.CFStateByAction[action].AutonomousSystemSet[*decision.Value] = struct{}{}
				}
			}
		}
	}
	worker.NewASDecisions = make([]*models.Decision, 0)
	return nil
}

func (worker *CloudflareWorker) DeleteASBans() error {
	decisionsByAction := dedupAndClassifyDecisionsByAction(worker.ExpiredASDecisions)
	for _, zoneCfg := range worker.Account.ZoneConfigs {
		zoneLogger := worker.Logger.WithFields(log.Fields{"zone_id": zoneCfg.ID})
		for action, decisions := range decisionsByAction {
			action = worker.normalizeActionForZone(action, zoneCfg)
			for _, decision := range decisions {
				if _, ok := worker.CFStateByAction[action].AutonomousSystemSet[*decision.Value]; ok {
					zoneLogger.Debugf("found expired AS ban for %s", *decision.Value)
					delete(worker.CFStateByAction[action].AutonomousSystemSet, *decision.Value)
				}
			}
		}
	}
	worker.ExpiredASDecisions = make([]*models.Decision, 0)
	return nil
}

func (worker *CloudflareWorker) normalizeActionForZone(action string, zoneCfg ZoneConfig) string {
	zoneLogger := worker.Logger.WithFields(log.Fields{"zone_id": zoneCfg.ID})
	if _, spAction := zoneCfg.ActionSet[action]; action == "defaulted" || !spAction {
		if action != "defaulted" {
			zoneLogger.Debugf("defaulting %s action to %s action", action, zoneCfg.Actions[0])
		}
		action = zoneCfg.Actions[0]
	}
	return action
}

func (worker *CloudflareWorker) SendCountryBans() error {
	decisionsByAction := dedupAndClassifyDecisionsByAction(worker.NewCountryDecisions)
	for _, zoneCfg := range worker.Account.ZoneConfigs {
		zoneLogger := worker.Logger.WithFields(log.Fields{"zone_id": zoneCfg.ID})
		for action, decisions := range decisionsByAction {
			action = worker.normalizeActionForZone(action, zoneCfg)
			for _, decision := range decisions {
				if _, ok := worker.CFStateByAction[action].CountrySet[*decision.Value]; !ok {
					zoneLogger.Debugf("found new country ban for %s", *decision.Value)
					worker.CFStateByAction[action].CountrySet[*decision.Value] = struct{}{}
				}
			}
		}
	}
	worker.NewCountryDecisions = make([]*models.Decision, 0)
	return nil
}

func (worker *CloudflareWorker) DeleteCountryBans() error {
	decisionsByAction := dedupAndClassifyDecisionsByAction(worker.ExpiredCountryDecisions)
	for _, zoneCfg := range worker.Account.ZoneConfigs {
		zoneLogger := worker.Logger.WithFields(log.Fields{"zone_id": zoneCfg.ID})
		for action, decisions := range decisionsByAction {
			action = worker.normalizeActionForZone(action, zoneCfg)
			for _, decision := range decisions {
				if _, ok := worker.CFStateByAction[action].CountrySet[*decision.Value]; ok {
					zoneLogger.Debugf("found expired country ban for %s", *decision.Value)
					delete(worker.CFStateByAction[action].CountrySet, *decision.Value)
				}
			}
		}
	}
	worker.ExpiredCountryDecisions = make([]*models.Decision, 0)
	return nil
}
func (worker *CloudflareWorker) UpdateRules() error {
	stateIsNew := false
	for action, state := range worker.CFStateByAction {
		if !worker.CFStateByAction[action].UpdateExpr() {
			// expression is still same, why bother.
			worker.Logger.Debugf("rule for %s action is unchanged", action)
			continue
		}
		stateIsNew = true
		for _, zone := range worker.Account.ZoneConfigs {
			zoneLogger := worker.Logger.WithFields(log.Fields{"zone_id": zone.ID})
			updatedFilters := make([]cloudflare.Filter, 0)
			if _, ok := zone.ActionSet[action]; ok {
				// check whether this action is supported by this zone
				updatedFilters = append(updatedFilters, cloudflare.Filter{ID: state.FilterIDByZoneID[zone.ID], Expression: state.CurrExpr})
			}
			if len(updatedFilters) > 0 {
				zoneLogger.Infof("updating %d rules", len(updatedFilters))
				_, err := worker.getAPI().UpdateFilters(worker.Ctx, zone.ID, updatedFilters)
				if err != nil {
					return err
				}
			} else {
				zoneLogger.Debug("rules are same")
			}
		}
	}
	if stateIsNew {
		go func() { worker.UpdatedState <- worker.CFStateByAction }()
	}
	return nil
}

func (worker *CloudflareWorker) runProcessorOnDecisions(processor func() error, decisions []*models.Decision) {
	if len(decisions) > 0 {
		worker.Logger.Infof("processing decisions with scope=%s", *decisions[0].Scope)
		err := processor()
		if err != nil {
			worker.Logger.Error(err)
		}
	}
}

func (worker *CloudflareWorker) Run() error {
	err := worker.Init()
	if err != nil {
		worker.Logger.Error(err.Error())
		return err
	}

	err = worker.SetUpCloudflareIfNewState()
	if err != nil {
		worker.Logger.Error(err.Error())
		return err
	}

	ticker := time.NewTicker(worker.UpdateFrequency)
	for {
		select {
		case <-ticker.C:
			worker.runProcessorOnDecisions(worker.DeleteIPs, worker.ExpiredIPDecisions)
			worker.runProcessorOnDecisions(worker.AddNewIPs, worker.NewIPDecisions)
			worker.runProcessorOnDecisions(worker.DeleteCountryBans, worker.ExpiredCountryDecisions)
			worker.runProcessorOnDecisions(worker.SendCountryBans, worker.NewCountryDecisions)
			worker.runProcessorOnDecisions(worker.DeleteASBans, worker.ExpiredASDecisions)
			worker.runProcessorOnDecisions(worker.SendASBans, worker.NewASDecisions)

			err := worker.UpdateRules()
			if err != nil {
				worker.Logger.Error(err)
				return err
			}

		case decisions := <-worker.LAPIStream:
			worker.Logger.Debug("collecting decisions from LAPI")
			worker.CollectLAPIStream(decisions)
		}
	}

}
