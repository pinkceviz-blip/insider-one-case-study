import { describe,it,expect } from 'vitest'
import { campaignPath,deliveryHistoryPath,isCampaignId,literalCount,templateError } from './security.js'
describe('frontend input boundaries',()=>{
 it.each(["' OR 1=1--",'../../metrics','x?channel=sms','javascript:alert(1)','%2e%2e%2fapi'])('rejects injected ID: %s',id=>{expect(isCampaignId(id)).toBe(false);expect(()=>campaignPath(id)).toThrow();expect(()=>deliveryHistoryPath(id)).toThrow()})
 it('builds only fixed API routes',()=>{const id='a'.repeat(32);expect(campaignPath(id,'/events')).toBe(`/api/campaigns/${id}/events`);expect(()=>campaignPath(id,'/../../metrics')).toThrow();expect(deliveryHistoryPath(`${id}-2-42-sms`)).toContain('/attempts')})
 it('keeps legitimate apostrophes and SQL-like copy as text',()=>{expect(templateError("O'Reilly SELECT fırsatları")).toBe('');expect(literalCount('ş'.repeat(160)+'{{first_name}}')).toBe(160);expect(templateError('{{email}}')).not.toBe('');expect(templateError('{{first_name')).not.toBe('')})
})
