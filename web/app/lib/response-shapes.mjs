const resourceKeys = {
  sandboxes: 'sandboxes',
  templates: 'namedSnapshots',
  environments: 'environments',
  'api-keys': 'apiKeys',
  webhooks: 'webhooks',
};

export function listResourceResponse(data, view) {
  if (Array.isArray(data)) return data;
  const key = resourceKeys[view];
  return key && data && typeof data === 'object' && Array.isArray(data[key]) ? data[key] : [];
}

export function createdAPIKeySecret(result) {
  return result?.apiKey?.key ?? result?.key ?? result?.token ?? '';
}
