/*
 * Tencent is pleased to support the open source community by making 蓝鲸 available.,
 * Copyright (C) 2017-2018 THL A29 Limited, a Tencent company. All rights reserved.
 * Licensed under the MIT License (the ",License",); you may not use this file except
 * in compliance with the License. You may obtain a copy of the License at
 * http://opensource.org/licenses/MIT
 * Unless required by applicable law or agreed to in writing, software distributed under
 * the License is distributed on an ",AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
 * either express or implied. See the License for the specific language governing permissions and
 * limitations under the License.
 */

package instances

import (
	"configcenter/src/common"
	"configcenter/src/common/blog"
	"configcenter/src/common/errors"
	"configcenter/src/common/http/rest"
	"configcenter/src/common/language"
	"configcenter/src/common/mapstr"
	"configcenter/src/common/metadata"
	"configcenter/src/common/util"
	"configcenter/src/source_controller/coreservice/core"
	"configcenter/src/storage/dal"
	"strings"

	redis "gopkg.in/redis.v5"
)

var _ core.InstanceOperation = (*instanceManager)(nil)

type instanceManager struct {
	dbProxy   dal.RDB
	dependent OperationDependences
	language  language.CCLanguageIf
	Cache     *redis.Client
}

// New create a new instance manager instance
func New(dbProxy dal.RDB, dependent OperationDependences, cache *redis.Client, language language.CCLanguageIf) core.InstanceOperation {
	return &instanceManager{
		dbProxy:   dbProxy,
		dependent: dependent,
		language:  language,
	}
}

func (m *instanceManager) instCnt(kit *rest.Kit, objID string, cond mapstr.MapStr) (cnt uint64, exists bool, err error) {
	tableName := common.GetInstTableName(objID)
	cnt, err = m.dbProxy.Table(tableName).Find(cond).Count(kit.Ctx)
	exists = 0 != cnt
	return cnt, exists, err
}

func (m *instanceManager) CreateModelInstance(kit *rest.Kit, objID string, inputParam metadata.CreateModelInstance) (*metadata.CreateOneDataResult, error) {
	rid := util.ExtractRequestIDFromContext(kit.Ctx)

	err := m.validCreateInstanceData(kit, objID, inputParam.Data)
	if nil != err {
		blog.Errorf("CreateModelInstance failed, valid error: %+v, rid: %s", err, rid)
		return nil, err
	}
	id, err := m.save(kit, objID, inputParam.Data)
	if err != nil {
		blog.ErrorJSON("CreateModelInstance create objID(%s) instance error. err:%s, data:%s, rid:%s", objID, err.Error(), inputParam.Data, kit.Rid)
		return nil, err
	}

	return &metadata.CreateOneDataResult{Created: metadata.CreatedDataResult{ID: id}}, err
}

func (m *instanceManager) CreateManyModelInstance(kit *rest.Kit, objID string, inputParam metadata.CreateManyModelInstance) (*metadata.CreateManyDataResult, error) {
	var newIDs []uint64
	dataResult := &metadata.CreateManyDataResult{}
	for itemIdx, item := range inputParam.Datas {
		item.Set(common.BKOwnerIDField, kit.SupplierAccount)
		err := m.validCreateInstanceData(kit, objID, item)
		if nil != err {
			dataResult.Exceptions = append(dataResult.Exceptions, metadata.ExceptionResult{
				Message:     err.Error(),
				Code:        int64(err.(errors.CCErrorCoder).GetCode()),
				Data:        item,
				OriginIndex: int64(itemIdx),
			})
			continue
		}
		item.Set(common.BKOwnerIDField, kit.SupplierAccount)
		id, err := m.save(kit, objID, item)
		if nil != err {
			dataResult.Exceptions = append(dataResult.Exceptions, metadata.ExceptionResult{
				Message:     err.Error(),
				Code:        int64(err.(errors.CCErrorCoder).GetCode()),
				Data:        item,
				OriginIndex: int64(itemIdx),
			})
			continue
		}

		dataResult.Created = append(dataResult.Created, metadata.CreatedDataResult{
			ID: id,
		})
		newIDs = append(newIDs, id)

	}

	return dataResult, nil
}

func (m *instanceManager) UpdateModelInstance(kit *rest.Kit, objID string, inputParam metadata.UpdateOption) (*metadata.UpdatedCount, error) {
	inputParam.Condition = util.SetModOwner(inputParam.Condition, kit.SupplierAccount)
	origins, _, err := m.getInsts(kit, objID, inputParam.Condition)
	if nil != err {
		blog.Errorf("UpdateModelInstance failed, get inst failed, err: %v, rid:%s", err, kit.Rid)
		return nil, err
	}

	if len(origins) == 0 {
		blog.Errorf("UpdateModelInstance failed, no instance found. model: %s, condition:%+v, rid:%s", objID, inputParam.Condition, kit.Rid)
		return nil, kit.CCError.Error(common.CCErrCommNotFound)
	}

	var instMedataData metadata.Metadata
	instMedataData.Label = make(metadata.Label)
	for key, val := range inputParam.Condition {
		if metadata.BKMetadata == key {
			bizID := metadata.GetBusinessIDFromMeta(val)
			if "" != bizID {
				instMedataData.Label.Set(metadata.LabelBusinessID, bizID)
			}
			break
		}
	}
	if inputParam.Condition.Exists(metadata.BKMetadata) {
		inputParam.Condition.Set(metadata.BKMetadata, instMedataData)
	}

	for _, origin := range origins {
		originCopy := origin.Clone()
		err := m.validUpdateInstanceData(kit, objID, inputParam.Data, instMedataData, originCopy, inputParam.CanEditAll)
		if nil != err {
			blog.Errorf("update model instance validate error :%v ,rid:%s", err, kit.Rid)
			return nil, err
		}
	}

	err = m.update(kit, objID, inputParam.Data, inputParam.Condition)
	if err != nil {
		blog.ErrorJSON("UpdateModelInstance update objID(%s) inst error. err:%s, condition:%s, rid:%s", objID, inputParam.Condition, kit.Rid)
		return nil, err
	}

	if objID == common.BKInnerObjIDHost {
		if err := m.updateHostProcessBindIP(kit, inputParam.Data, origins); err != nil {
			return nil, err
		}
	}

	return &metadata.UpdatedCount{Count: uint64(len(origins))}, nil
}

// updateHostProcessBindIP if hosts' ips are updated, update processes which binds the changed ip
func (m *instanceManager) updateHostProcessBindIP(kit *rest.Kit, updateData mapstr.MapStr, origins []mapstr.MapStr) error {
	innerIP, innerIPExist := updateData[common.BKHostInnerIPField]
	outerIP, outerIPExist := updateData[common.BKHostOuterIPField]
	innerIPUpdated, outerIPUpdated := false, false

	firstInnerIP := getFirstIP(innerIP)
	firstOuterIP := getFirstIP(outerIP)

	// get all hosts whose first ip changes
	innerIPUpdatedHostIDs := make([]int64, 0)
	outerIPUpdatedHostIDs := make([]int64, 0)
	for _, origin := range origins {
		if innerIPExist && getFirstIP(origin[common.BKHostInnerIPField]) != firstInnerIP {
			innerIPUpdated = true
			hostID, err := util.GetInt64ByInterface(origin[common.BKHostIDField])
			if err != nil {
				blog.Errorf("host ID invalid, err: %v, host: %+v, rid: %s", err, origin, kit.Rid)
				return err
			}
			innerIPUpdatedHostIDs = append(innerIPUpdatedHostIDs, hostID)
		}

		if outerIPExist && getFirstIP(origin[common.BKHostOuterIPField]) != firstOuterIP {
			outerIPUpdated = true
			hostID, err := util.GetInt64ByInterface(origin[common.BKHostIDField])
			if err != nil {
				blog.Errorf("host ID invalid, err: %v, host: %+v, rid: %s", err, origin, kit.Rid)
				return err
			}
			outerIPUpdatedHostIDs = append(outerIPUpdatedHostIDs, hostID)
		}
	}

	if innerIPUpdated {
		if err := m.updateProcessBindIP(kit, firstInnerIP, true, innerIPUpdatedHostIDs); err != nil {
			blog.Errorf("update process bind inner ip failed, err: %v, inner ip: %s, hosts: %+v, rid: %s", err, innerIP, innerIPUpdatedHostIDs, kit.Rid)
			return err
		}
	}

	if outerIPUpdated {
		if err := m.updateProcessBindIP(kit, firstOuterIP, false, outerIPUpdatedHostIDs); err != nil {
			blog.Errorf("update process bind outer ip failed, err: %v, outer ip: %s, hosts: %+v, rid: %s", err, innerIP, outerIPUpdatedHostIDs, kit.Rid)
			return err
		}
	}

	return nil
}

func getFirstIP(ip interface{}) string {
	switch t := ip.(type) {
	case string:
		index := strings.Index(t, ",")
		if index == -1 {
			return t
		}

		return t[:index]
	case []string:
		if len(t) == 0 {
			return ""
		}

		return t[0]
	case []interface{}:
		if len(t) == 0 {
			return ""
		}

		return util.GetStrByInterface(t[0])
	}
	return util.GetStrByInterface(ip)
}

// updateHostProcessBindIP update processes using changed ip
func (m *instanceManager) updateProcessBindIP(kit *rest.Kit, ip string, isInner bool, hostIDs []int64) error {
	// get hosts related process and template relations
	processRelations := make([]metadata.ProcessInstanceRelation, 0)
	processRelationFilter := map[string]interface{}{common.BKHostIDField: map[string]interface{}{common.BKDBIN: hostIDs}}

	err := m.dbProxy.Table(common.BKTableNameProcessInstanceRelation).Find(processRelationFilter).Fields(
		common.BKHostIDField, common.BKProcessIDField, common.BKProcessTemplateIDField).All(kit.Ctx, &processRelations)
	if err != nil {
		blog.Errorf("get process relation failed, err: %v, hostIDs: %+v, rid: %s", err, hostIDs, kit.Rid)
		return err
	}

	if len(processRelations) == 0 {
		return nil
	}

	processTemplateIDs := make([]int64, len(processRelations))
	processTemplateMap := make(map[int64][]int64)
	for index, relation := range processRelations {
		processTemplateIDs[index] = relation.ProcessTemplateID
		processTemplateMap[relation.ProcessTemplateID] = append(processTemplateMap[relation.ProcessTemplateID], relation.ProcessID)
	}

	// get all processes whose templates has corresponding bind ip
	processTemplates := make([]metadata.ProcessTemplate, 0)
	processTemplateFilter := map[string]interface{}{
		common.BKFieldID:                    map[string]interface{}{common.BKDBIN: processTemplateIDs},
		"property.bind_ip.as_default_value": true,
	}

	if isInner {
		processTemplateFilter["property.bind_ip.value"] = metadata.BindInnerIP
	} else {
		processTemplateFilter["property.bind_ip.value"] = metadata.BindOtterIP
	}

	err = m.dbProxy.Table(common.BKTableNameProcessTemplate).Find(processTemplateFilter).Fields(
		common.BKFieldID).All(kit.Ctx, &processTemplates)
	if err != nil {
		blog.Errorf("get process template failed, err: %v, processTemplateIDs: %+v, rid: %s", err, processTemplateIDs, kit.Rid)
		return err
	}

	processIDs := make([]int64, 0)
	for _, processTemplate := range processTemplates {
		processIDs = append(processIDs, processTemplateMap[processTemplate.ID]...)
	}

	if len(processIDs) == 0 {
		return nil
	}

	// update all processes bind ip
	processFilter := map[string]interface{}{common.BKProcessIDField: map[string]interface{}{common.BKDBIN: processIDs}}
	bindIPData := map[string]interface{}{common.BKBindIP: ip}

	if err = m.dbProxy.Table(common.BKTableNameBaseProcess).Update(kit.Ctx, processFilter, bindIPData); err != nil {
		blog.Errorf("update process failed, err: %v, processIDs: %+v, ip: %s, rid: %s", err, processIDs, ip, kit.Rid)
		return err
	}

	return nil
}

func (m *instanceManager) SearchModelInstance(kit *rest.Kit, objID string, inputParam metadata.QueryCondition) (*metadata.QueryResult, error) {
	blog.V(9).Infof("search instance with parameter: %+v, rid: %s", inputParam, kit.Rid)

	tableName := common.GetInstTableName(objID)
	if tableName == common.BKTableNameBaseInst {
		if inputParam.Condition == nil {
			inputParam.Condition = mapstr.MapStr{}
		}
		objIDCond, ok := inputParam.Condition[common.BKObjIDField]
		if ok && objIDCond != objID {
			blog.V(9).Infof("searchInstance condition's bk_obj_id: %s not match objID: %s, rid: %s", objIDCond, objID, kit.Rid)
			return nil, nil
		}
		inputParam.Condition[common.BKObjIDField] = objID
	}
	inputParam.Condition = util.SetQueryOwner(inputParam.Condition, kit.SupplierAccount)

	instItems := make([]mapstr.MapStr, 0)
	query := m.dbProxy.Table(tableName).Find(inputParam.Condition).Start(uint64(inputParam.Page.Start)).
		Limit(uint64(inputParam.Page.Limit)).
		Sort(inputParam.Page.Sort).
		Fields(inputParam.Fields...)
	var instErr error
	if objID == common.BKInnerObjIDHost {
		hosts := make([]metadata.HostMapStr, 0)
		instErr = query.All(kit.Ctx, &hosts)
		for _, host := range hosts {
			instItems = append(instItems, mapstr.MapStr(host))
		}
	} else {
		instErr = query.All(kit.Ctx, &instItems)
	}
	if instErr != nil {
		blog.Errorf("search instance error [%v], rid: %s", instErr, kit.Rid)
		return nil, instErr
	}

	count, countErr := m.dbProxy.Table(tableName).Find(inputParam.Condition).Count(kit.Ctx)
	if countErr != nil {
		blog.Errorf("count instance error [%v], rid: %s", countErr, kit.Rid)
		return nil, countErr
	}

	dataResult := &metadata.QueryResult{
		Count: count,
		Info:  instItems,
	}

	return dataResult, nil
}

func (m *instanceManager) DeleteModelInstance(kit *rest.Kit, objID string, inputParam metadata.DeleteOption) (*metadata.DeletedCount, error) {
	tableName := common.GetInstTableName(objID)
	instIDFieldName := common.GetInstIDField(objID)
	inputParam.Condition.Set(common.BKOwnerIDField, kit.SupplierAccount)
	inputParam.Condition = util.SetModOwner(inputParam.Condition, kit.SupplierAccount)

	origins, _, err := m.getInsts(kit, objID, inputParam.Condition)
	if nil != err {
		return &metadata.DeletedCount{}, err
	}

	for _, origin := range origins {
		instID, err := util.GetInt64ByInterface(origin[instIDFieldName])
		if nil != err {
			return nil, err
		}
		exists, err := m.dependent.IsInstAsstExist(kit, objID, uint64(instID))
		if nil != err {
			return nil, err
		}
		if exists {
			return &metadata.DeletedCount{}, kit.CCError.Error(common.CCErrorInstHasAsst)
		}
	}

	err = m.dbProxy.Table(tableName).Delete(kit.Ctx, inputParam.Condition)
	if nil != err {
		blog.ErrorJSON("DeleteModelInstance delete objID(%s) instance error. err:%s, coniditon:%s, rid:%s", objID, err.Error(), inputParam.Condition, kit.Rid)
		return &metadata.DeletedCount{}, err
	}

	return &metadata.DeletedCount{Count: uint64(len(origins))}, nil
}

func (m *instanceManager) CascadeDeleteModelInstance(kit *rest.Kit, objID string, inputParam metadata.DeleteOption) (*metadata.DeletedCount, error) {
	tableName := common.GetInstTableName(objID)
	instIDFieldName := common.GetInstIDField(objID)
	origins, _, err := m.getInsts(kit, objID, inputParam.Condition)
	blog.V(5).Infof("cascade delete model instance get inst error:%v, rid: %s", origins, kit.Rid)
	if nil != err {
		blog.Errorf("cascade delete model instance get inst error:%v, rid: %s", err, kit.Rid)
		return &metadata.DeletedCount{}, err
	}

	for _, origin := range origins {
		instID, err := util.GetInt64ByInterface(origin[instIDFieldName])
		if nil != err {
			return &metadata.DeletedCount{}, err
		}
		err = m.dependent.DeleteInstAsst(kit, objID, uint64(instID))
		if nil != err {
			return &metadata.DeletedCount{}, err
		}
	}
	inputParam.Condition = util.SetModOwner(inputParam.Condition, kit.SupplierAccount)
	err = m.dbProxy.Table(tableName).Delete(kit.Ctx, inputParam.Condition)
	if nil != err {
		return &metadata.DeletedCount{}, err
	}
	return &metadata.DeletedCount{Count: uint64(len(origins))}, nil
}
