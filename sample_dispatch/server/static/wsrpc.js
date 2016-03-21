//// Javascript lib to communicate with Go wsrpc server
// This lib support only sending requests and receiving responses (client side).
// It was designed to work in events based environment, as such you may specify
// a callback to receive returned data from the remote call.
//
// You connect to a server that way:
//
//  a = new wsrpc.Node('ws://localhost:12345/web')
//
// When creating an instance you can also register callbacks for each events:
// On open connection, on connection fails, on close connection, and on error
//
//  a = new wsrpc.Node(
// 		server,			// server address
// 		onopen,			// function(node)
// 		onconnectfail,	// function(node)
// 		onclose,		// function(node)
// 		onerror			// function(node, error)
//  )
//
// You call a remote function that way:
//
//  a.remote_call('MyService.MyFunc')
//
// You may add keyword argument that way:
//
//  a.remote_call('MyService.MyFunc', {'a':1,'b':2})
//
// To register a callback:
//
//  a.remote_call('MyService.MyFunc', null, my_callback)
//
// The callback need to take 2 arguments. the first one is the Node instance,
// and the second is the returned object:
//
// function my_callback(node, reply)

function _wsrpc()
{
	var isset = function(v) { return v != undefined }
	var _last_id = 0;
	var generate_id = function()
	{ // limit to unsigned 32-bit integer and avoid 0 (0 id mean no response)
		return ++_last_id >>> 0 || ++_last_id >>> 0;
	}

	this.Node = function(
			server,
			onopen_cb,
			onconnectfail_cb,
			onclose_cb,
			onerror_cb
		)
	{
	//// Private
		var _ws = new WebSocket(server);
		var _parent = this;
		var _connected = false;
		_ws.onopen = function()
		{
			_connected = true;
			if(onopen_cb) onopen_cb(_parent);
		}
		_ws.onclose = function()
		{
			if(!_connected)
			{
				if(onconnectfail_cb)
				{
					onconnectfail_cb(_parent);
					return;
				}
				else
					_onerror('Could not connect to server '+server);
			}
			_connected = false;
			if(onclose_cb) onclose_cb(_parent);
		}
		_ws.onerror = function(e)
		{
			_onerror(e.data);
		}
		_ws.onmessage = function (e)
		{
			var r_object = JSON.parse(e.data);
			var id = r_object['ID'];
			if(!isset(id)) return;
			var callback = _callbacks[id];
			delete _callbacks[id];
			var error = r_object['SV'] == 'ERR';
			if(error)
				_onerror("Remote Exception:\n"+ r_object['KW']);
			else
			{ // assume response (SV == 'R')
				if(isset(callback))
				{
					var value = r_object['KW'];
					if(isset(value)) callback(_parent, value);
				}
			}
		};
		var _callbacks = new Object();
		var _onerror = function(data)
		{
			if(onerror_cb) onerror_cb(_parent, data);
			else throw new Error(data);
		}
	//// Public
		this.close = function() { _ws.close(); }
		this.remote_call = function (name, kwargs, callback)
		{
			if(_connected)
			{
				var id = isset(callback)? generate_id() : 0;
				if(id) _callbacks[id] = callback;
				var call_object = {ID:id, SV:name, KW:kwargs};
				_ws.send(JSON.stringify(call_object));
			}
			else throw new Error('Not connected to server '+server);
		}
	}
}

var wsrpc = new _wsrpc();
