// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go#L5
typedef Chandef {
	chan sync = [0] of {bool};
	chan enq = [0] of {bool};
	chan deq = [0] of {bool,bool};
	chan sending = [0] of {bool};
	chan rcving = [0] of {bool};
	chan closing = [0] of {bool};
	int size = 0;
	int num_msgs = 0;
	bool closed = false;
}



init { 
	chan child_EffectReceiptBeforeAdoption50 = [1] of {int}->
	run EffectReceiptBeforeAdoption5(child_EffectReceiptBeforeAdoption50)->
	child_EffectReceiptBeforeAdoption50?0->
stop_process:skip
}

proctype EffectReceiptBeforeAdoption5(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousEffectReceiptBeforeAdoption1584 = [1] of {int}->
	chan child_AnonymousEffectReceiptBeforeAdoption1463 = [1] of {int}->
	chan child_AnonymousEffectReceiptBeforeAdoption1372 = [1] of {int}->
	chan child_AnonymousEffectReceiptBeforeAdoption1291 = [1] of {int}->
	chan child_AnonymousEffectReceiptBeforeAdoption1160 = [1] of {int}->
	Chandef adopted_ch->
	Chandef authority_ch->
	Chandef receipt_ch->
	Chandef performed_ch->
	Chandef intent_ch->
	run sync_monitor(intent_ch)->
	run sync_monitor(performed_ch)->
	run sync_monitor(receipt_ch)->
	run sync_monitor(authority_ch)->
	run sync_monitor(adopted_ch)->
	run AnonymousEffectReceiptBeforeAdoption116(intent_ch,performed_ch,receipt_ch,authority_ch,adopted_ch,child_AnonymousEffectReceiptBeforeAdoption1160)->
	run receiver(child_AnonymousEffectReceiptBeforeAdoption1160)->
	run AnonymousEffectReceiptBeforeAdoption129(authority_ch,adopted_ch,intent_ch,performed_ch,receipt_ch,child_AnonymousEffectReceiptBeforeAdoption1291)->
	run receiver(child_AnonymousEffectReceiptBeforeAdoption1291)->
	run AnonymousEffectReceiptBeforeAdoption137(performed_ch,receipt_ch,authority_ch,adopted_ch,intent_ch,child_AnonymousEffectReceiptBeforeAdoption1372)->
	run receiver(child_AnonymousEffectReceiptBeforeAdoption1372)->
	run AnonymousEffectReceiptBeforeAdoption146(intent_ch,performed_ch,receipt_ch,authority_ch,adopted_ch,child_AnonymousEffectReceiptBeforeAdoption1463)->
	run receiver(child_AnonymousEffectReceiptBeforeAdoption1463)->
	run AnonymousEffectReceiptBeforeAdoption158(receipt_ch,authority_ch,adopted_ch,intent_ch,performed_ch,child_AnonymousEffectReceiptBeforeAdoption1584)->
	run receiver(child_AnonymousEffectReceiptBeforeAdoption1584)->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:16:2 */
	:: adopted_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:16:2 */->
	:: adopted_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:16:2 */ -> 
		adopted_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousEffectReceiptBeforeAdoption116(Chandef intent_ch;Chandef performed_ch;Chandef receipt_ch;Chandef authority_ch;Chandef adopted_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:11:14 */
	:: intent_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:11:14 */
	:: intent_ch.sync!false -> 
		intent_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:11:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousEffectReceiptBeforeAdoption129(Chandef authority_ch;Chandef adopted_ch;Chandef intent_ch;Chandef performed_ch;Chandef receipt_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:12:14 */
	:: intent_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:12:14 */->
	:: intent_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:12:14 */ -> 
		intent_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:12:24 */
	:: performed_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:12:24 */
	:: performed_ch.sync!false -> 
		performed_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:12:24 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousEffectReceiptBeforeAdoption137(Chandef performed_ch;Chandef receipt_ch;Chandef authority_ch;Chandef adopted_ch;Chandef intent_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:13:14 */
	:: performed_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:13:14 */->
	:: performed_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:13:14 */ -> 
		performed_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:13:27 */
	:: receipt_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:13:27 */
	:: receipt_ch.sync!false -> 
		receipt_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:13:27 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousEffectReceiptBeforeAdoption146(Chandef intent_ch;Chandef performed_ch;Chandef receipt_ch;Chandef authority_ch;Chandef adopted_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:14:14 */
	:: authority_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:14:14 */
	:: authority_ch.sync!false -> 
		authority_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:14:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousEffectReceiptBeforeAdoption158(Chandef receipt_ch;Chandef authority_ch;Chandef adopted_ch;Chandef intent_ch;Chandef performed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:15:14 */
	:: receipt_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:15:14 */->
	:: receipt_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:15:14 */ -> 
		receipt_ch.rcving!false
	fi->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:15:25 */
	:: authority_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:15:25 */->
	:: authority_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:15:25 */ -> 
		authority_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:15:38 */
	:: adopted_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:15:38 */
	:: adopted_ch.sync!false -> 
		adopted_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/effect.go:15:38 */
	fi->
	stop_process: skip->
	child!0
}

 /* ================================================================================== */
 /* ================================================================================== */
 /* ================================================================================== */ 
proctype async_monitor(Chandef ch) {
do
:: true ->
if
:: ch.closed -> 
end: if
  :: ch.num_msgs > 0 -> // cannot send on closed channel
    end4: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.deq!true,ch.num_msgs -> 
  		 ch.num_msgs = ch.num_msgs - 1
    fi->
  :: else ->    end5: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.sync!true-> 
    fi->
  fi->
:: else ->
	if
	:: ch.num_msgs == ch.size ->
		end1: if
		  :: ch.deq!false,ch.num_msgs ->
		    ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		    ch.closed = true
		   :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	:: ch.num_msgs == 0 -> 
end2:		if
		:: ch.enq!true -> // a message has been received
			ch.num_msgs = ch.num_msgs + 1
		:: ch.closing!false -> // closing the channel
			ch.closed = true
		:: ch.rcving?false ->
 		    ch.sending!true->
		fi->
		:: else -> 
		end3: if
		  :: ch.enq!true->
		     ch.num_msgs = ch.num_msgs + 1
		  :: ch.deq!false,ch.num_msgs->
		     ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		     ch.closed = true
		  :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	fi->
fi->
od->
}

proctype sync_monitor(Chandef ch) {
do
:: true->
if
:: ch.closed ->
end: if
  :: ch.enq!false-> // cannot send on closed channel
  :: ch.closing!true -> // cannot close twice a channel
  :: ch.sending!false -> // sending state of channel (closed)
  :: ch.rcving?false->
  :: ch.sync!true-> // can always receive on a closed chan
  fi->
:: else -> 
end1: if
    :: ch.rcving?false ->
       ch.sending!true->
    :: ch.closing!false ->
      ch.closed = true
    fi->
fi->
od->
stop_process: skip
}

proctype receiver(chan c) {
c?0
}

