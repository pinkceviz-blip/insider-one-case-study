// Input is data. Do not blacklist SQL words or remove apostrophes from copy.
export const isCampaignId = id => typeof id === 'string' && /^[0-9a-f]{32}$/.test(id)
export function campaignPath(id, suffix = '') {
 if (!isCampaignId(id) || !['','/events','/approve'].includes(suffix)) throw new Error('Geçersiz kampanya kimliği')
 return `/api/campaigns/${encodeURIComponent(id)}${suffix}`
}
export function deliveryHistoryPath(id) {
 if (typeof id !== 'string' || !/^[A-Za-z0-9_-]{1,200}$/.test(id)) throw new Error('Geçersiz mesaj kimliği')
 return `/api/deliveries/${encodeURIComponent(id)}/attempts`
}
export const literalCount = text => Array.from((text || '').replace(/\{\{\s*first_name\s*\}\}/g,'')).length
export function templateError(text) {
 const literal = (text || '').replace(/\{\{\s*first_name\s*\}\}/g,'')
 return literal.includes('{{') || literal.includes('}}') ? 'Yalnız {{first_name}} değişkeni kullanılabilir.' : ''
}
