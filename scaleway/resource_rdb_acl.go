package scaleway

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sort"

	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/scaleway/scaleway-sdk-go/api/rdb/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
	"github.com/scaleway/terraform-provider-scaleway/v2/internal/locality"
	"github.com/scaleway/terraform-provider-scaleway/v2/internal/locality/regional"
)

func resourceScalewayRdbACL() *schema.Resource {
	return &schema.Resource{
		CreateContext: resourceScalewayRdbACLCreate,
		ReadContext:   resourceScalewayRdbACLRead,
		UpdateContext: resourceScalewayRdbACLUpdate,
		DeleteContext: resourceScalewayRdbACLDelete,
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},
		Timeouts: &schema.ResourceTimeout{
			Create:  schema.DefaultTimeout(defaultRdbInstanceTimeout),
			Read:    schema.DefaultTimeout(defaultRdbInstanceTimeout),
			Update:  schema.DefaultTimeout(defaultRdbInstanceTimeout),
			Delete:  schema.DefaultTimeout(defaultRdbInstanceTimeout),
			Default: schema.DefaultTimeout(defaultRdbInstanceTimeout),
		},
		SchemaVersion: 0,
		Schema: map[string]*schema.Schema{
			"instance_id": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validationUUIDorUUIDWithLocality(),
				Description:  "Instance on which the ACL is applied",
			},
			"acl_rules": {
				Type:        schema.TypeList,
				Required:    true,
				Description: "List of ACL rules to apply",
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"ip": {
							Type:         schema.TypeString,
							ValidateFunc: validation.IsCIDR,
							Required:     true,
							Description:  "Target IP of the rules",
						},
						"description": {
							Type:        schema.TypeString,
							Optional:    true,
							Computed:    true,
							Description: "Description of the rule",
						},
					},
				},
			},
			// Common
			"region": regional.Schema(),
		},
		CustomizeDiff: CustomizeDiffLocalityCheck("instance_id"),
	}
}

func resourceScalewayRdbACLCreate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	api, region, err := rdbAPIWithRegion(d, m)
	if err != nil {
		return diag.FromErr(err)
	}

	instanceID := d.Get("instance_id").(string)
	_, err = waitForRDBInstance(ctx, api, region, locality.ExpandID(instanceID), d.Timeout(schema.TimeoutCreate))
	if err != nil {
		return diag.FromErr(err)
	}

	aclRules, err := rdbACLExpand(d.Get("acl_rules").([]interface{}))
	if err != nil {
		return diag.FromErr(err)
	}
	createReq := &rdb.SetInstanceACLRulesRequest{
		Region:     region,
		InstanceID: locality.ExpandID(instanceID),
		Rules:      aclRules,
	}

	_, err = api.SetInstanceACLRules(createReq, scw.WithContext(ctx))
	if err != nil {
		return diag.FromErr(err)
	}

	d.SetId(instanceID)

	return resourceScalewayRdbACLRead(ctx, d, m)
}

func resourceScalewayRdbACLRead(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	rdbAPI, region, instanceID, err := rdbAPIWithRegionAndID(m, d.Id())
	if err != nil {
		return diag.FromErr(err)
	}

	_, err = waitForRDBInstance(ctx, rdbAPI, region, instanceID, d.Timeout(schema.TimeoutRead))
	if err != nil && !is404Error(err) {
		return diag.FromErr(err)
	}

	res, err := rdbAPI.ListInstanceACLRules(&rdb.ListInstanceACLRulesRequest{
		Region:     region,
		InstanceID: instanceID,
	}, scw.WithContext(ctx))
	if err != nil {
		if is404Error(err) {
			d.SetId("")
			return nil
		}
		return diag.FromErr(err)
	}

	id := regional.NewID(region, instanceID).String()
	d.SetId(id)
	_ = d.Set("instance_id", id)

	diags := diag.Diagnostics{}

	if aclRulesRaw, ok := d.GetOk("acl_rules"); ok {
		aclRules, mergeErrors := rdbACLRulesFlattenFromSchema(res.Rules, aclRulesRaw.([]interface{}))
		if len(mergeErrors) > 0 {
			for _, w := range mergeErrors {
				diags = append(diags, diag.Diagnostic{
					Severity:      diag.Warning,
					Summary:       "acl_rules does not match server's, updating state",
					Detail:        w.Error(),
					AttributePath: cty.GetAttrPath("acl_rules"),
				})
			}
		}
		_ = d.Set("acl_rules", aclRules)
	} else {
		_ = d.Set("acl_rules", rdbACLRulesFlatten(res.Rules))
	}
	_ = d.Set("region", region)

	return diags
}

func resourceScalewayRdbACLUpdate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	rdbAPI, region, instanceID, err := rdbAPIWithRegionAndID(m, d.Id())
	if err != nil {
		return diag.FromErr(err)
	}

	_, err = waitForRDBInstance(ctx, rdbAPI, region, instanceID, d.Timeout(schema.TimeoutUpdate))
	if err != nil && !is404Error(err) {
		return diag.FromErr(err)
	}

	if d.HasChange("acl_rules") {
		_, err := waitForRDBInstance(ctx, rdbAPI, region, instanceID, d.Timeout(schema.TimeoutUpdate))
		if err != nil {
			return diag.FromErr(err)
		}

		aclRules, err := rdbACLExpand(d.Get("acl_rules").([]interface{}))
		if err != nil {
			return diag.FromErr(err)
		}
		req := &rdb.SetInstanceACLRulesRequest{
			Region:     region,
			InstanceID: instanceID,
			Rules:      aclRules,
		}

		_, err = rdbAPI.SetInstanceACLRules(req, scw.WithContext(ctx))
		if err != nil {
			return diag.FromErr(err)
		}
	}

	return resourceScalewayRdbACLRead(ctx, d, m)
}

func resourceScalewayRdbACLDelete(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	rdbAPI, region, instanceID, err := rdbAPIWithRegionAndID(m, d.Id())
	if err != nil {
		return diag.FromErr(err)
	}
	aclRuleIPs := make([]string, 0)
	aclRules, err := rdbACLExpand(d.Get("acl_rules").([]interface{}))
	if err != nil {
		return diag.FromErr(err)
	}
	for _, acl := range aclRules {
		aclRuleIPs = append(aclRuleIPs, acl.IP.String())
	}

	_, err = waitForRDBInstance(ctx, rdbAPI, region, instanceID, d.Timeout(schema.TimeoutDelete))
	if err != nil && !is404Error(err) {
		return diag.FromErr(err)
	}

	_, err = rdbAPI.DeleteInstanceACLRules(&rdb.DeleteInstanceACLRulesRequest{
		Region:     region,
		InstanceID: instanceID,
		ACLRuleIPs: aclRuleIPs,
	}, scw.WithContext(ctx))
	if err != nil && !is404Error(err) {
		return diag.FromErr(err)
	}

	_, err = waitForRDBInstance(ctx, rdbAPI, region, instanceID, d.Timeout(schema.TimeoutDelete))
	if err != nil && !is404Error(err) {
		return diag.FromErr(err)
	}

	return nil
}

func rdbACLExpand(data []interface{}) ([]*rdb.ACLRuleRequest, error) {
	var res []*rdb.ACLRuleRequest
	for _, rule := range data {
		r := rule.(map[string]interface{})

		ipRaw, ok := r["ip"]
		if ok {
			aclRule := &rdb.ACLRuleRequest{}
			ip, err := expandIPNet(ipRaw.(string))
			if err != nil {
				return res, err
			}
			aclRule.IP = ip
			if descriptionRaw, descriptionExist := r["description"]; descriptionExist {
				aclRule.Description = descriptionRaw.(string)
			}
			res = append(res, aclRule)
		}
	}
	sort.Slice(res, func(i, j int) bool {
		return bytes.Compare(res[i].IP.IP, res[j].IP.IP) < 0
	})

	return res, nil
}

func rdbACLRulesFlattenFromSchema(rules []*rdb.ACLRule, dataFromSchema []interface{}) ([]map[string]interface{}, []error) {
	res := make([]map[string]interface{}, 0, len(dataFromSchema))
	var errors []error
	ruleMap := make(map[string]*rdb.ACLRule)
	for _, rule := range rules {
		ruleMap[rule.IP.String()] = rule
	}

	ruleMapFromSchema := map[string]struct{}{}
	for _, ruleFromSchema := range dataFromSchema {
		currentRule := ruleFromSchema.(map[string]interface{})
		ip, err := expandIPNet(currentRule["ip"].(string))
		if err != nil {
			errors = append(errors, err)
			continue
		}

		aclRule, aclRuleExists := ruleMap[ip.String()]
		if !aclRuleExists {
			errors = append(errors, fmt.Errorf("acl from state does not exist on server (%s)", ip.String()))
			continue
		}
		ruleMapFromSchema[ip.String()] = struct{}{}
		r := map[string]interface{}{
			"ip":          aclRule.IP.String(),
			"description": aclRule.Description,
		}
		res = append(res, r)
	}

	return append(res, mergeDiffToSchema(ruleMapFromSchema, ruleMap)...), errors
}

func mergeDiffToSchema(rulesFromSchema map[string]struct{}, ruleMap map[string]*rdb.ACLRule) []map[string]interface{} {
	var res []map[string]interface{}

	for ruleIP, info := range ruleMap {
		_, ok := rulesFromSchema[ruleIP]
		// check if new rule has been added on config
		if !ok {
			r := map[string]interface{}{
				"ip":          info.IP.String(),
				"description": info.Description,
			}
			res = append(res, r)
		}
	}

	return res
}

func rdbACLRulesFlatten(rules []*rdb.ACLRule) []map[string]interface{} {
	res := make([]map[string]interface{}, 0, len(rules))
	for _, rule := range rules {
		r := map[string]interface{}{
			"ip":          rule.IP.String(),
			"description": rule.Description,
		}
		res = append(res, r)
	}

	sort.Slice(res, func(i, j int) bool {
		ipI, _, _ := net.ParseCIDR(res[i]["ip"].(string))
		ipJ, _, _ := net.ParseCIDR(res[j]["ip"].(string))
		return bytes.Compare(ipI, ipJ) < 0
	})
	return res
}
